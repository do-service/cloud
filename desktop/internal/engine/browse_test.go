package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/do-service/cloud/desktop/internal/state"
)

func TestMarksAndUsage(t *testing.T) {
	h, s := fakeHarness(t)
	s.put("Фото/2026/a.jpg", "aa")
	s.put("Фото/2026/b.jpg", "bbb")
	s.put("Фото/old.jpg", "o")
	s.put("Документы/d.txt", "d")
	ctx := context.Background()
	if _, err := h.e.Marks(ctx, nil); !errors.Is(err, ErrNotReady) {
		t.Fatalf("marks before Prepare: %v", err)
	}
	if err := h.e.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	tr, _, err := h.e.loadTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := func(p string) string {
		n, err := tr.resolve(p)
		if err != nil {
			t.Fatal(err)
		}
		return n.NodeID
	}
	photo, y2026, a, old, docs := id("/Фото"), id("/Фото/2026"), id("/Фото/2026/a.jpg"), id("/Фото/old.jpg"), id("/Документы")
	marks := func() map[string]Mark {
		m, err := h.e.Marks(ctx, []string{photo, y2026, a, old, docs, "gone"})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := m["gone"]; ok {
			t.Fatal("a mark for a node the mirror lacks")
		}
		return m
	}

	if m := marks(); m[photo] != (Mark{Check: CheckOff}) || m[a] != (Mark{Check: CheckOff}) {
		t.Fatalf("nothing selected: %+v", m)
	}
	if _, err := h.e.SelectNode(ctx, y2026); err != nil {
		t.Fatal(err)
	}
	m := marks()
	if m[photo] != (Mark{CheckPartial, SyncPending}) || m[y2026] != (Mark{CheckOn, SyncPending}) || m[a] != (Mark{CheckOn, SyncPending}) || m[old].Check != CheckOff {
		t.Fatalf("selected, not synced yet: %+v", m)
	}
	if u, err := h.e.Usage(ctx); err != nil || u.Selected != 5 {
		t.Fatalf("usage %+v, %v", u, err)
	}
	h.sync()
	m = marks()
	if m[photo] != (Mark{CheckPartial, SyncDone}) || m[a] != (Mark{CheckOn, SyncDone}) || m[docs].Sync != SyncNone {
		t.Fatalf("synced: %+v", m)
	}

	// A file unchecked in a checked folder makes the folder partial.
	if _, err := h.e.SelectNode(ctx, photo); err != nil {
		t.Fatal(err)
	}
	if err := h.e.UnselectNode(ctx, old); err != nil {
		t.Fatal(err)
	}
	h.sync()
	m = marks()
	if m[photo].Check != CheckPartial || m[y2026].Check != CheckOn || m[old].Check != CheckOff {
		t.Fatalf("checked with an unchecked file: %+v", m)
	}

	// A problem below a folder is the folder's error.
	if err := h.db.Update(ctx, func(tx *state.Tx) error {
		_, err := tx.PutProblem(state.Problem{Kind: ProblemLocked, RelPath: "Фото/2026/a.jpg"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m = marks()
	if m[photo].Sync != SyncError || m[y2026].Sync != SyncError || m[a].Sync != SyncError {
		t.Fatalf("with a problem: %+v", m)
	}
}
