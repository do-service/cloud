package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSelectUnselect(t *testing.T) {
	h, s := fakeHarness(t)
	s.put("Фото/2026/a.jpg", "a")
	s.put("Фото/2026/b.jpg", "b")
	s.put("Фото/old.jpg", "o")
	s.put("Документы/d.txt", "d")
	ctx := context.Background()
	if err := h.e.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	need, err := h.e.Select(ctx, "/фото/2026")
	if err != nil || need != 2 {
		t.Fatalf("select: %d, %v", need, err)
	}
	h.sync()
	got := localTree(t, h.root)
	if len(got) != 4 || got["Фото/2026/a.jpg"] == "" || got["Фото/old.jpg"] != "" {
		t.Fatalf("after selecting a subfolder: %v", got)
	}
	// Фото is a container: a file put there by hand is uploaded and
	// stays selected.
	h.write("Фото/моё.txt", "m")
	h.sync()
	if s.tree()["Фото/моё.txt"] == "" {
		t.Fatal("a new file in a container wasn't uploaded")
	}
	sel, _ := h.e.Selection(ctx)
	if len(sel) != 2 {
		t.Fatalf("selection %+v", sel)
	}

	// Unchecking: an edit not uploaded yet goes up first, then the
	// folder leaves the disk; nothing is deleted on the server.
	h.write("Фото/2026/a.jpg", "a-edited")
	if err := h.e.Unselect(ctx, "/Фото/2026"); err != nil {
		t.Fatal(err)
	}
	h.sync()
	if _, err := os.Stat(filepath.Join(h.root, "Фото", "2026")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unchecked folder still on disk: %v", err)
	}
	if s.tree()["Фото/2026/a.jpg"] != shaOf("a-edited") || s.tree()["Фото/2026/b.jpg"] != shaOf("b") {
		t.Fatalf("server after unchecking: %v", s.tree())
	}
	if h.read("Фото/моё.txt") != "m" {
		t.Fatal("the separately included file went too")
	}

	// Not enough free space: refused, nothing selected.
	freeSpaceOf = func(string) (int64, bool) { return 0, true }
	defer func() { freeSpaceOf = freeSpace }()
	if _, err := h.e.Select(ctx, "/Документы"); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("select without space: %v", err)
	}
	if _, err := h.e.Select(ctx, "/нет такого"); !errors.Is(err, ErrNotInCloud) {
		t.Fatalf("select a missing path: %v", err)
	}
}

// Run: a file written while the engine runs is on the server within
// seconds, and a server change arrives by the poll.
func TestRunWatches(t *testing.T) {
	s := newFakeServer()
	h := newHarness(t, s, "u1", func(c *Config) { c.FallbackRescan = 200 * time.Millisecond })
	defer h.close()
	h.selectAll()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.e.Run(ctx) }()
	h.write("живое.txt", "1")
	s.put("с сервера.txt", "2")
	deadline := time.Now().Add(10 * time.Second)
	for s.tree()["живое.txt"] == "" || localTree(t, h.root)["с сервера.txt"] == "" {
		if time.Now().After(deadline) {
			t.Fatalf("not synced while running: server %v, local %v", s.tree(), localTree(t, h.root))
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
