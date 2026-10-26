package watch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The native watcher of this platform, through Debounce, as the engine
// uses it: new files, a folder moved in with contents and a renamed
// folder end up as dirty paths, and a change inside the renamed folder
// afterwards is reported under its new name (inotify keeps a watch per
// folder, which must follow the rename).
func TestWatcherDirtyPaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "old", "deep"), 0o755))
	w, err := New(root, Options{Since: SinceNow, Latency: 50 * time.Millisecond})
	if errors.Is(err, ErrUnsupported) {
		t.Skip(err)
	}
	must(err)
	defer w.Close()
	dirty := Debounce(w.Root(), w.Events(), 200*time.Millisecond, time.Second)

	// waitFor reads bursts until each wanted path is covered by a dirty
	// one (equal or an ancestor). FSEvents may still report the temp
	// dir's own creation at first, a whole-root rescan ("") that covers
	// everything: a valid, if costly, answer, allowed only in the first
	// phase.
	waitFor := func(allowRoot bool, want ...string) {
		t.Helper()
		seen := map[string]bool{}
		deadline := time.After(10 * time.Second)
		for len(seen) < len(want) {
			select {
			case d := <-dirty:
				for _, p := range d.Paths {
					if p == "" {
						if !allowRoot {
							t.Fatalf("whole-root rescan: %+v", d)
						}
						for _, w := range want {
							seen[w] = true
						}
					}
					if p == "old/deep" || strings.HasPrefix(p, "old/deep/") {
						t.Fatalf("event under the old name of a renamed folder: %q", p)
					}
					for _, w := range want {
						if w == p || strings.HasPrefix(w, p+"/") {
							seen[w] = true
						}
					}
				}
			case <-deadline:
				t.Fatalf("reported %v of %q", seen, want)
			}
		}
	}

	must(os.WriteFile(filepath.Join(root, "new.txt"), []byte("x"), 0o644))
	must(os.MkdirAll(filepath.Join(outside, "pack", "inner"), 0o755))
	must(os.WriteFile(filepath.Join(outside, "pack", "inner", "f.txt"), []byte("f"), 0o644))
	must(os.Rename(filepath.Join(outside, "pack"), filepath.Join(root, "pack")))
	must(os.Rename(filepath.Join(root, "old"), filepath.Join(root, "renamed")))
	waitFor(true, "new.txt", "pack", "old", "renamed")

	must(os.WriteFile(filepath.Join(root, "renamed", "deep", "later.txt"), []byte("l"), 0o644))
	waitFor(false, "renamed/deep/later.txt")
}
