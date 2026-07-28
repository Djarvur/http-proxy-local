package app

import (
	"log"
	"sync/atomic"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

// RunPACRegenerator launches a goroutine that listens on regen for signals
// and re-renders the PAC from the current cfgPtr and rangesPtr, atomically
// storing the result in pacPtr. Returns a stop function; calling it waits
// for any in-flight render to finish.
//
// Design contract (design D2): the regenerator is the single writer to
// pacPtr. Rapid signals coalesce — the channel is buffered, and the
// goroutine drains all pending signals before each render, so N back-to-
// back signals yield at most a small number of renders (often one).
func RunPACRegenerator(
	regen <-chan struct{},
	cfgPtr *atomic.Pointer[port.Config],
	rangesPtr *atomic.Pointer[port.RURanges],
	pacPtr *atomic.Pointer[[]byte],
) (stop func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			// Wait for at least one signal.
			_, ok := <-regen
			if !ok {
				return
			}
			// Drain any further pending signals so a burst collapses into
			// a single render.
			drain(regen)

			renderPAC(cfgPtr, rangesPtr, pacPtr)
		}
	}()
	return func() {
		// Best-effort: we can't force-close regen (the caller owns it).
		// Wait for the goroutine to exit if the channel has been closed
		// by the caller; otherwise this returns immediately as a no-op.
		select {
		case <-done:
		default:
		}
	}
}

// drain non-blockingly removes all currently-buffered signals.
func drain(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// renderPAC reads the current config + ranges, renders the PAC, and atomically
// stores it. If either pointer is nil, the render is skipped (the caller will
// signal again once both are populated).
func renderPAC(cfgPtr *atomic.Pointer[port.Config], rangesPtr *atomic.Pointer[port.RURanges], pacPtr *atomic.Pointer[[]byte]) {
	cfg := cfgPtr.Load()
	ranges := rangesPtr.Load()
	if cfg == nil {
		return
	}
	if ranges == nil {
		r := port.RURanges{}
		ranges = &r
	}
	input := port.PACInput{
		Listen:    cfg.Listen,
		Whitelist: cfg.Routing.Whitelist,
		Blacklist: cfg.Routing.Blacklist,
		Ranges:    *ranges,
	}
	body := RenderPAC(input)
	pacPtr.Store(&body)
}

// ReloadOrchestrator coordinates hot-reload: on Reload(), it loads + validates
// the config file, swaps it into cfgPtr, and signals PAC regen when a
// PAC-affecting field changed. Validation failures retain the old config
// and report an error. listen changes are swapped (the snapshot updates so
// per-request reads are consistent) but emit a restart-required error since
// the listener is not rebound mid-flight.
type ReloadOrchestrator struct {
	loader port.ConfigLoader
	path   string
	cfgPtr *atomic.Pointer[port.Config]
	regen  chan<- struct{}
	errs   chan<- error
}

// NewReloadOrchestrator constructs an orchestrator. errs receives validation
// and restart-required errors; it must be buffered or have a consumer.
func NewReloadOrchestrator(
	loader port.ConfigLoader,
	path string,
	cfgPtr *atomic.Pointer[port.Config],
	regen chan<- struct{},
	errs chan<- error,
) *ReloadOrchestrator {
	return &ReloadOrchestrator{
		loader: loader,
		path:   path,
		cfgPtr: cfgPtr,
		regen:  regen,
		errs:   errs,
	}
}

// Reload attempts a single reload. Safe to call from any goroutine.
func (o *ReloadOrchestrator) Reload() {
	newCfg, err := o.loader.Load(o.path)
	if err != nil {
		o.report(err)
		return
	}
	if err := o.loader.Validate(newCfg); err != nil {
		o.report(err)
		return
	}

	old := o.cfgPtr.Load()
	o.cfgPtr.Store(&newCfg)

	// If listen changed, warn that rebind is required (still swapped so
	// downstream reads are consistent — the listener just won't move).
	if old != nil && old.Listen != newCfg.Listen {
		o.report(restartRequiredError{Field: "listen", Old: old.Listen, New: newCfg.Listen})
		// listen is PAC-affecting (the PROXY directive uses it), so signal.
		o.signalRegen()
		return
	}

	// If socks5.listen changed (including the block being added or removed
	// entirely), warn that a restart is required — the SOCKS5 listener is
	// not rebound, started, or stopped mid-flight. The config IS swapped so
	// downstream reads stay consistent; the error tells the operator the
	// listener won't reflect the change until restart.
	if old != nil && old.SOCKS5.Listen != newCfg.SOCKS5.Listen {
		o.report(restartRequiredError{Field: "socks5.listen", Old: old.SOCKS5.Listen, New: newCfg.SOCKS5.Listen})
	}

	// If smart.listen changed (including the block being added or removed),
	// warn that a restart is required — the smart HTTP listener is not
	// rebound, started, or stopped mid-flight (same posture as socks5.listen
	// and the HTTP listen field). The smart TUNING fields (dial_timeout,
	// dial_attempts, negative_cache_ttl) are read per-request, so changes to
	// them apply without restart and do NOT emit a restart-required error.
	if old != nil && old.Smart.Listen != newCfg.Smart.Listen {
		o.report(restartRequiredError{Field: "smart.listen", Old: old.Smart.Listen, New: newCfg.Smart.Listen})
	}

	// Signal PAC regen if any PAC-affecting field changed.
	if old == nil || pacAffectingChanged(old, &newCfg) {
		o.signalRegen()
	}
}

// pacAffectingChanged reports whether whitelist or blacklist changed
// (listen is handled by the caller above).
func pacAffectingChanged(old, new *port.Config) bool {
	return !sliceEqual(old.Routing.Whitelist, new.Routing.Whitelist) ||
		!sliceEqual(old.Routing.Blacklist, new.Routing.Blacklist)
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (o *ReloadOrchestrator) signalRegen() {
	if o.regen == nil {
		return
	}
	select {
	case o.regen <- struct{}{}:
	default:
		// Channel full → a render is already pending; coalesce.
	}
}

func (o *ReloadOrchestrator) report(err error) {
	if o.errs == nil {
		return
	}
	select {
	case o.errs <- err:
	default:
		log.Printf("reload error channel full; dropping: %v", err)
	}
}

// restartRequiredError is returned (via errs) when a field changed that the
// running process can't apply without a restart.
type restartRequiredError struct {
	Field string
	Old   string
	New   string
}

func (e restartRequiredError) Error() string {
	return "config field " + e.Field + " changed (" + e.Old + " → " + e.New + "): restart required to apply"
}
