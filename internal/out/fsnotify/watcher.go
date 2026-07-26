// Package fsnotify implements a debounced config-file watcher. Editors save
// via write-temp-then-rename and produce bursts of inotify events within
// tens of milliseconds; this package collapses a burst into a single
// reload signal on its C() channel.
package fsnotify

import (
	"context"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher watches a single file path and emits a debounced signal on C()
// whenever the file changes (write, create, or rename into the path).
//
// One goroutine owns the debounce timer; events from fsnotify reset it. When
// it fires (i.e. `debounce` has elapsed since the last event), exactly one
// signal is sent on C().
type Watcher struct {
	out chan struct{}
}

// New creates a Watcher on path with the given debounce window. It watches
// path's parent directory (not just the file) so rename-into-path is caught
// — fsnotify on a single inode misses the rename case on many platforms.
//
// The watcher runs until ctx is cancelled, at which point C() is closed.
func New(ctx context.Context, path string, debounce time.Duration) (*Watcher, error) {
	w := &Watcher{out: make(chan struct{}, 1)}

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	dir := filepath.Dir(path)
	if err := fw.Add(dir); err != nil {
		_ = fw.Close()
		return nil, err
	}

	go w.loop(ctx, fw, path, debounce)
	return w, nil
}

// C returns the channel that receives a signal after each debounced change.
// The channel is buffered(1); extra signals during a slow consumer coalesce.
func (w *Watcher) C() <-chan struct{} { return w.out }

// loop is the single owner of the debounce timer. It selects on:
//   - ctx cancellation → shut down
//   - fsnotify events that match path → reset the timer
//   - timer fire → emit signal, disarm
//
// Reset semantics: only one timer in flight at any time; the most recent
// matching event wins (last-write-wins, the correct debounce behavior).
func (w *Watcher) loop(ctx context.Context, fw *fsnotify.Watcher, path string, debounce time.Duration) {
	defer func() {
		_ = fw.Close()
		close(w.out)
	}()

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}

	var timer *time.Timer
	var timerC <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-fw.Events:
			if !ok {
				return
			}
			if !w.matches(ev, abs) {
				continue
			}
			timer, timerC = armTimer(timer, timerC, debounce)

		case <-timerC:
			// Disarm and emit. Non-blocking send: a slow consumer that hasn't
			// drained the previous signal just keeps the stale one (one
			// reload is enough; we don't queue).
			timerC = nil
			select {
			case w.out <- struct{}{}:
			default:
			}

		case _, ok := <-fw.Errors:
			if !ok {
				return
			}
			// Non-fatal: keep watching. The next save re-triggers.
		}
	}
}

// armTimer creates the timer on first use, or stops+resets it on subsequent
// matches. Returns the (possibly new) timer and its updated channel.
func armTimer(timer *time.Timer, timerC <-chan time.Time, debounce time.Duration) (*time.Timer, <-chan time.Time) {
	if timer == nil {
		t := time.NewTimer(debounce)
		return t, t.C
	}
	if !timer.Stop() {
		// Timer already fired; drain its value to clear timerC.
		select {
		case <-timerC:
		default:
		}
	}
	timer.Reset(debounce)
	return timer, timer.C
}

// matches reports whether ev is a change to the watched path (write, create,
// or rename). We compare cleaned absolute paths so rename-into-path is
// caught even though we watch the directory.
func (w *Watcher) matches(ev fsnotify.Event, absPath string) bool {
	evAbs, err := filepath.Abs(ev.Name)
	if err != nil {
		evAbs = ev.Name
	}
	if evAbs != absPath {
		return false
	}
	return ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) || ev.Has(fsnotify.Rename)
}
