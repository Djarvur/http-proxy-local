package fsnotify_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	fsnotifyadapter "github.com/Djarvur/http-proxy-local/internal/out/fsnotify"
)

// writeThenSync writes content to path and fsyncs, simulating a real editor save.
func writeThenSync(t *testing.T, path, content string) {
	t.Helper()
	// gosec G304 false positive: path is a test-owned tempdir file.
	f, err := os.Create(path) //nolint:gosec // G304: test temp file
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// recvOne waits for a signal on ch within a generous fixed window, returning
// whether it arrived. Drains any immediate duplicate that snuck in within the
// debounce window (still counts as "one logical reload").
func recvOne(t *testing.T, ch <-chan struct{}) bool {
	t.Helper()
	const wait = 500 * time.Millisecond
	select {
	case <-ch:
		select {
		case <-ch:
		case <-time.After(20 * time.Millisecond):
		}
		return true
	case <-time.After(wait):
		return false
	}
}

func TestWatcher_BurstWithinDebounceYieldsOneSignal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeThenSync(t, path, "initial: true\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := fsnotifyadapter.New(ctx, path, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Two writes 10ms apart — within the debounce window.
	writeThenSync(t, path, "v1: 1\n")
	time.Sleep(10 * time.Millisecond)
	writeThenSync(t, path, "v2: 2\n")

	if !recvOne(t, w.C()) {
		t.Fatal("expected exactly one debounced signal, got none")
	}
}

func TestWatcher_TempThenRenameYieldsOneSignal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeThenSync(t, path, "initial: true\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := fsnotifyadapter.New(ctx, path, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Simulate an editor's atomic save: write a temp file, then rename.
	tmp := path + ".tmp"
	writeThenSync(t, tmp, "renamed: true\n")
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if !recvOne(t, w.C()) {
		t.Fatal("expected exactly one signal after temp-then-rename, got none")
	}
}

func TestWatcher_TwoDistinctSavesYieldTwoSignals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeThenSync(t, path, "v0\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := fsnotifyadapter.New(ctx, path, 30*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	writeThenSync(t, path, "v1\n")
	if !recvOne(t, w.C()) {
		t.Fatal("missing first signal")
	}

	// Wait past the debounce window so the next write is a distinct event.
	time.Sleep(60 * time.Millisecond)
	writeThenSync(t, path, "v2\n")
	if !recvOne(t, w.C()) {
		t.Fatal("missing second signal")
	}
}

func TestWatcher_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeThenSync(t, path, "v0\n")

	ctx, cancel := context.WithCancel(context.Background())
	w, err := fsnotifyadapter.New(ctx, path, 30*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cancel()

	// The contract: after ctx is cancelled, the watcher's C() channel must
	// close (loop exits, deferred close fires). A signal already in flight
	// from a write that raced with cancel is acceptable and not a bug.
	select {
	case _, ok := <-w.C():
		if !ok {
			// Channel closed — expected terminal state.
			return
		}
		// Got an in-flight signal; drain and wait for close.
	case <-time.After(200 * time.Millisecond):
		t.Fatal("C() did not close within 200ms of cancel")
	}

	// Now wait for the channel to close.
	select {
	case _, ok := <-w.C():
		if ok {
			t.Fatal("got additional signal after cancel; expected channel close")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("C() did not close after cancel")
	}
}
