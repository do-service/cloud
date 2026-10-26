//go:build darwin && cgo

package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A tree of a few thousand files, as a sync folder has: the watcher must
// see changes deep in it without being told about every directory.
func makeTree(t *testing.T, root string, dirs, files int) {
	t.Helper()
	for d := range dirs {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", d), "nested")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := range files {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", f)), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func tempRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// collect reads batches until want reports every expected path seen, and
// returns the last event id it got.
func collect(t *testing.T, w Watcher, want map[string]Op) uint64 {
	t.Helper()
	var last uint64
	deadline := time.After(10 * time.Second)
	for len(want) > 0 {
		select {
		case batch, ok := <-w.Events():
			if !ok {
				t.Fatal("events closed")
			}
			for _, e := range batch {
				if e.ID > last && e.ID != SinceNow {
					last = e.ID
				}
				if op, ok := want[e.Path]; ok && e.Op&op == op {
					delete(want, e.Path)
				}
			}
		case <-deadline:
			t.Fatalf("no events for %v", want)
		}
	}
	return last
}

func TestWatcherReportsChangesInLargeTree(t *testing.T) {
	root := tempRoot(t)
	makeTree(t, root, 30, 100)
	w, err := New(root, Options{Since: SinceNow, Latency: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.Root() != root {
		t.Fatalf("Root() = %q, want %q", w.Root(), root)
	}

	created := filepath.Join(root, "d05", "nested", "new.txt")
	if err := os.WriteFile(created, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	from := filepath.Join(root, "d10", "nested", "f001.txt")
	to := filepath.Join(root, "d11", "renamed.txt")
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	removed := filepath.Join(root, "d29", "nested", "f099.txt")
	if err := os.Remove(removed); err != nil {
		t.Fatal(err)
	}
	newDir := filepath.Join(root, "fresh", "deep")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	inNewDir := filepath.Join(newDir, "a.txt")
	if err := os.WriteFile(inNewDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	collect(t, w, map[string]Op{
		created:  Create | IsFile,
		from:     Rename | IsFile,
		to:       Rename | IsFile,
		removed:  Remove | IsFile,
		newDir:   Create | IsDir,
		inNewDir: Create | IsFile,
	})
}

func TestWatcherReplaysFromSavedID(t *testing.T) {
	root := tempRoot(t)
	makeTree(t, root, 3, 10)
	// What a client persists at shutdown: the id of the last event it
	// handled, or the current one when nothing happened.
	saved := CurrentEventID()
	if saved == 0 {
		t.Fatal("CurrentEventID() = 0")
	}

	// Changes while no watcher runs, as when the app is closed.
	offline := filepath.Join(root, "d01", "nested", "offline.txt")
	if err := os.WriteFile(offline, []byte("o"), 0o644); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(root, "d02", "nested", "f005.txt")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	// FSEvents writes its history asynchronously; a watcher started at
	// once could see the change live rather than replayed, which would
	// still pass but not test the replay.
	time.Sleep(time.Second)

	w, err := New(root, Options{Since: saved, Latency: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	var sawHistoryDone bool
	want := map[string]Op{offline: Create | IsFile, gone: Remove | IsFile}
	deadline := time.After(10 * time.Second)
	for !sawHistoryDone || len(want) > 0 {
		select {
		case batch := <-w.Events():
			for _, e := range batch {
				if e.Op.Has(HistoryDone) {
					sawHistoryDone = true
					if len(want) > 0 {
						t.Fatalf("history done before %v", want)
					}
					continue
				}
				if e.ID <= saved {
					t.Errorf("replayed event %d at or before the saved id %d", e.ID, saved)
				}
				if op, ok := want[e.Path]; ok && e.Op&op == op {
					delete(want, e.Path)
				}
			}
		case <-deadline:
			t.Fatalf("history done %v, missing %v", sawHistoryDone, want)
		}
	}
}

func TestCloseUnblocksSlowConsumer(t *testing.T) {
	root := tempRoot(t)
	w, err := New(root, Options{Since: SinceNow, Latency: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// Nobody reads: the channel fills and the callback blocks in deliver.
	for i := range 200 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() {
		w.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
}
