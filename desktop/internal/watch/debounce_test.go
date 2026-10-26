package watch

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestDebounceCollapses(t *testing.T) {
	root := filepath.FromSlash("/sync/root")
	in := make(chan []Event)
	out := Debounce(root, in, 50*time.Millisecond, time.Second)
	abs := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }
	in <- []Event{
		{Path: abs("a/b/c.txt"), Op: Modify, ID: 5},
		{Path: abs("a/b"), Op: Rename | IsDir, ID: 9},
		{Path: abs("a b"), Op: Create, ID: 7},
		{Path: abs("z.txt"), Op: Create, ID: 8},
		{Path: filepath.FromSlash("/sync/rootless/x"), Op: Create, ID: 3}, // outside root
		{Path: abs("Café/é.txt"), Op: Create},
	}
	in <- []Event{{Path: abs("a/b/new"), Op: Create}, {Path: abs("z.txt"), Op: Modify}}
	d := <-out
	want := []string{"Café/é.txt", "a b", "a/b", "z.txt"}
	if !slices.Equal(d.Paths, want) || d.LastID != 9 || d.RootChanged || d.HistoryDone {
		t.Fatalf("Dirty = %+v, want paths %q", d, want)
	}

	// A rescan of the root covers everything; root changes and history
	// end are flags, not paths.
	in <- []Event{
		{Path: abs("x"), Op: Create},
		{Path: root, Op: MustRescan},
		{Path: root, Op: RootChanged},
		{Op: HistoryDone, ID: SinceNow},
	}
	close(in)
	d = <-out
	if !slices.Equal(d.Paths, []string{""}) || !d.RootChanged || !d.HistoryDone || d.LastID != 9 {
		t.Fatalf("Dirty = %+v", d)
	}
	if _, ok := <-out; ok {
		t.Fatal("out not closed after in")
	}
}

// Events every 20 ms with a quiet time of 100 ms never go quiet: maxDelay
// still flushes.
func TestDebounceMaxDelay(t *testing.T) {
	in := make(chan []Event)
	out := Debounce("/r", in, 100*time.Millisecond, 250*time.Millisecond)
	start := time.Now()
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case in <- []Event{{Path: "/r/growing.bin", Op: Modify}}:
			case <-stop:
				close(in)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	d := <-out
	close(stop)
	if el := time.Since(start); el < 200*time.Millisecond || el > 600*time.Millisecond {
		t.Fatalf("first flush after %v", el)
	}
	if !slices.Equal(d.Paths, []string{"growing.bin"}) {
		t.Fatalf("Dirty = %+v", d)
	}
	for range out {
	}
}
