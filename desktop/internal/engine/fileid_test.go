package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// A state database of a client that stored file ids without birth times
// ("volume:index"), and a file moved while it was off: after the upgrade
// nothing is transferred again, the move is a move, and on a file system
// with birth times base gets them.
func TestFileIDsWithoutBirth(t *testing.T) {
	h, s := fakeHarness(t)
	s.put("a.txt", "a")
	s.put("d/b.txt", "b")
	s.put("m.txt", "m")
	h.selectAll()
	h.sync()
	moved := s.paths()["m.txt"]

	// As the old client wrote them.
	ctx := context.Background()
	if err := h.db.Update(ctx, func(tx *state.Tx) error {
		var base []state.Base
		var local []state.Local
		if err := tx.EachBase(func(b state.Base) error { base = append(base, b); return nil }); err != nil {
			return err
		}
		if err := tx.EachLocal(func(l state.Local) error { local = append(local, l); return nil }); err != nil {
			return err
		}
		for _, b := range base {
			b.FileID = localfs.FileIDKey(b.FileID)
			if err := tx.PutBase(b); err != nil {
				return err
			}
		}
		for _, l := range local {
			l.FileID = localfs.FileIDKey(l.FileID)
			if err := tx.PutLocal(l); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if err := os.Rename(filepath.Join(h.root, "m.txt"), filepath.Join(h.root, "d", "m2.txt")); err != nil {
		t.Fatal(err)
	}

	transfers := 0
	s.fault = func(m string) (bool, bool) {
		if m == "upload" || m == "download" {
			transfers++
		}
		return false, false
	}
	h.sync()
	expectSame(t, h, s)
	if transfers != 0 {
		t.Errorf("%d transfers after the upgrade", transfers)
	}
	if got := s.paths()["d/m2.txt"]; got != moved {
		t.Errorf("d/m2.txt is node %s, not the moved %s", got, moved)
	}

	st, err := localfs.Stat(h.root, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if st.FileID.Birth == 0 {
		return // no birth times here: ids stay as they were
	}
	if err := h.db.View(ctx, func(tx *state.Tx) error {
		return tx.EachBase(func(b state.Base) error {
			if b.FileID != "" && strings.Count(b.FileID, ":") != 2 {
				t.Errorf("%s: %s without a birth time", b.RelPath, b.FileID)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}
