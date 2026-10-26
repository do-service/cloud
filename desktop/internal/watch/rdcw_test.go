//go:build windows

package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func startRDCW(t *testing.T, root string) Watcher {
	t.Helper()
	w, err := New(root, Options{Since: SinceNow, Latency: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

// waitEvent reads batches until one has an event that ok accepts.
func waitEvent(t *testing.T, w Watcher, what string, ok func(Event) bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case batch := <-w.Events():
			for _, e := range batch {
				if ok(e) {
					return
				}
			}
		case <-deadline:
			t.Fatalf("no %s", what)
		}
	}
}

// The reason for one recursive watch: folders at any depth, and the
// parents of folders with changes in them, stay renamable while watched.
// fsnotify's handle per folder made these renames fail with "Access is
// denied".
func TestRDCWRenameWatchedFolders(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	w := startRDCW(t, root)
	if err := os.WriteFile(filepath.Join(deep, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, "event for a/b/c/f.txt", func(e Event) bool {
		return e.Path == filepath.Join(w.Root(), "a", "b", "c", "f.txt") && e.Op.Has(Create)
	})
	if err := os.Rename(deep, filepath.Join(root, "a", "b", "c2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "a2")); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, "rename of a to a2", func(e Event) bool {
		return e.Path == filepath.Join(w.Root(), "a2") && e.Op.Has(Rename) && e.Op.Has(IsDir)
	})
}

// A buffer too small for a single record makes every read an overflow,
// which must come out as a rescan of the root.
func TestRDCWOverflowRescans(t *testing.T) {
	defer func(n int) { bufferSize = n }(bufferSize)
	bufferSize = 16
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := startRDCW(t, root)
	if err := os.WriteFile(filepath.Join(root, "a-name-longer-than-the-buffer.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, "rescan of the root", func(e Event) bool {
		return e.Path == w.Root() && e.Op.Has(MustRescan)
	})
}

// Windows says nothing when the watched folder itself is renamed; the
// root check notices.
func TestRDCWRootRenamed(t *testing.T) {
	defer func(d time.Duration) { rootCheckInterval = d }(rootCheckInterval)
	rootCheckInterval = 50 * time.Millisecond
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	w := startRDCW(t, root)
	if err := os.Rename(root, filepath.Join(parent, "moved")); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, "root change", func(e Event) bool {
		return e.Path == w.Root() && e.Op.Has(RootChanged)
	})
}
