package localfs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A waiter returns only after a flush that started after its Add and
// covered its folder; waiters arriving together share flushes; a failed
// flush fails its waiters and is retried for the next.
func TestSyncerGroupCommit(t *testing.T) {
	var (
		mu      sync.Mutex
		flushed []string
		calls   atomic.Int64
		fail    atomic.Bool
	)
	var g group
	g.init(func(dirs []string) error {
		calls.Add(1)
		time.Sleep(5 * time.Millisecond) // a flush takes a while: waiters pile up
		if fail.Load() {
			return errors.New("EIO")
		}
		mu.Lock()
		flushed = append(flushed, dirs...)
		mu.Unlock()
		return nil
	})
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := filepath.Join("/x", string(rune('a'+i%8)))
			g.add(d)
			if err := g.wait(); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Contains(flushed, d) {
				t.Errorf("%s not flushed when its Wait returned", d)
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n >= 32 {
		t.Errorf("%d flushes for 64 waiters: not batched", n)
	}

	fail.Store(true)
	failing := filepath.Join("/x", "failing")
	g.add(failing)
	if err := g.wait(); err == nil {
		t.Fatal("a failed flush didn't fail its waiter")
	}
	fail.Store(false)
	mu.Lock()
	flushed = nil
	mu.Unlock()
	if err := g.wait(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(flushed, failing) {
		t.Fatalf("a folder whose flush failed wasn't flushed again: %v", flushed)
	}
}

// The real flushes, on this OS: Commit through a Syncer puts the file in
// place, Wait flushes, and the same with folders removed meanwhile.
func TestSyncerCommit(t *testing.T) {
	root := t.TempDir()
	s := NewSyncer()
	if err := s.Mkdir(root, "d"); err != nil {
		t.Fatal(err)
	}
	part, err := PartPath(root, "d/a.txt", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(root, "d/a.txt", part, CommitOptions{Sync: s}); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir(root, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "gone")); err != nil {
		t.Fatal(err)
	}
	s.Add(filepath.Join(root, "gone"))
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "d", "a.txt")); err != nil || string(b) != "a" {
		t.Fatalf("%q, %v", b, err)
	}
}
