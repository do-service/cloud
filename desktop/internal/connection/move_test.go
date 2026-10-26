package connection

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/state"
)

// withPart records a resumable download into conn's folder, whose
// absolute .part path must follow the move.
func withPart(t *testing.T, conn state.Connection) {
	t.Helper()
	path, _ := state.Path(conn.ID)
	db, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(context.Background(), func(tx *state.Tx) error {
		id, err := tx.AddOp(state.Op{Kind: "download", Path: "Docs/a.txt", State: state.OpPending})
		if err != nil {
			return err
		}
		return tx.PutTransfer(state.Transfer{OpID: id, PartPath: filepath.Join(conn.LocalRoot, "Docs", ".~cloud-n1.part")})
	}); err != nil {
		t.Fatal(err)
	}
}

// expectAt checks that conn's folder, files, registry entry and state.db
// are all at root.
func expectAt(t *testing.T, base string, conn state.Connection, root string) {
	t.Helper()
	got, err := state.OpenRegistry(base).Get(conn.ID)
	if err != nil || got.LocalRoot != root || got.MovingTo != "" {
		t.Fatalf("registry %+v, %v; want at %s", got, err, root)
	}
	if err := localfs.CheckMarker(root, conn.ID); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "Docs", "a.txt")); err != nil || string(b) != "a" {
		t.Fatalf("the files: %q, %v", b, err)
	}
	path, _ := state.Path(conn.ID)
	db, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.View(context.Background(), func(tx *state.Tx) error {
		acc, err := tx.Account()
		if err != nil || acc.LocalRoot != root {
			t.Errorf("account %+v, %v", acc, err)
		}
		ops, _ := tx.Ops()
		x, err := tx.Transfer(ops[0].ID)
		if want := filepath.Join(root, "Docs", ".~cloud-n1.part"); err != nil || x.PartPath != want {
			t.Errorf("part %q, %v; want %s", x.PartPath, err, want)
		}
		return nil
	})
}

func TestMoveFolderChecks(t *testing.T) {
	home, base := testBase(t)
	conn := setup(t, base, "u1", filepath.Join(home, "Cloud", "a"))
	write(t, filepath.Join(conn.LocalRoot, "Docs", "a.txt"), "a")
	other := setup(t, base, "u2", filepath.Join(home, "Cloud", "b"))
	full := filepath.Join(home, "full")
	write(t, filepath.Join(full, "x.txt"), "x")
	junk := filepath.Join(home, "junk")
	write(t, filepath.Join(junk, "Thumbs.db"), "x")
	ctx := context.Background()

	for _, tc := range []struct {
		dir  string
		want error
	}{
		{conn.LocalRoot, ErrInsideCurrent},
		{filepath.Join(conn.LocalRoot, "Docs", "new"), ErrInsideCurrent},
		{filepath.Join(other.LocalRoot, "in"), state.ErrNested},
		{filepath.Join(home, "Cloud"), state.ErrNested}, // around both
		{home, state.ErrNested},
		{base, state.ErrNested},
		{full, ErrFolderNotEmpty},
		// Ignored by sync, but the user's: removing the folder for the
		// rename would delete it.
		{junk, ErrFolderNotEmpty},
		{filepath.Join(full, "x.txt"), ErrNotFolder},
	} {
		if _, err := MoveFolder(ctx, base, conn, tc.dir); !errors.Is(err, tc.want) {
			t.Fatalf("move to %s: %v, want %v", tc.dir, err, tc.want)
		}
	}
	// The engine still has it: refused, nothing changed.
	lock, err := state.LockConnection(conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MoveFolder(ctx, base, conn, filepath.Join(home, "new")); !errors.Is(err, state.ErrLocked) {
		t.Fatalf("move while open: %v", err)
	}
	_ = lock.Unlock()
	if got, _ := state.OpenRegistry(base).Get(conn.ID); got.LocalRoot != conn.LocalRoot || got.MovingTo != "" {
		t.Fatalf("after refusals: %+v", got)
	}
	if err := localfs.CheckMarker(conn.LocalRoot, conn.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMoveFolder(t *testing.T) {
	home, base := testBase(t)
	conn := setup(t, base, "u1", filepath.Join(home, "Cloud", "a"))
	write(t, filepath.Join(conn.LocalRoot, "Docs", "a.txt"), "a")
	withPart(t, conn)
	ctx := context.Background()

	// To a new folder, parents created.
	to := filepath.Join(home, "Disk", "Облако")
	moved, err := MoveFolder(ctx, base, conn, to)
	if err != nil || moved.LocalRoot != to {
		t.Fatalf("move: %+v, %v", moved, err)
	}
	expectAt(t, base, conn, to)
	if _, err := os.Stat(conn.LocalRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old folder: %v", err)
	}

	// Into an empty folder Finder has been in: it is replaced.
	picked := filepath.Join(home, "Picked")
	write(t, filepath.Join(picked, ".DS_Store"), "x")
	if moved, err = MoveFolder(ctx, base, moved, picked); err != nil {
		t.Fatal(err)
	}
	expectAt(t, base, conn, picked)
	if _, err := os.Stat(filepath.Join(picked, ".DS_Store")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Finder's .DS_Store came along: %v", err)
	}
}

// A crash after each step of a move: the next Open finishes it, and the
// engine finds its folder, files and marker in the new place.
func TestMoveFolderCrash(t *testing.T) {
	ctx := context.Background()
	for _, step := range []string{"begun", "cleared", "renamed", "recorded"} {
		t.Run(step, func(t *testing.T) {
			home, base := testBase(t)
			conn := setup(t, base, "u1", filepath.Join(home, "Cloud", "a"))
			write(t, filepath.Join(conn.LocalRoot, "Docs", "a.txt"), "a")
			withPart(t, conn)
			to := filepath.Join(home, "Picked")
			write(t, filepath.Join(to, ".DS_Store"), "x")
			crash(t, step)
			if _, err := MoveFolder(ctx, base, conn, to); !errors.Is(err, errCrash) {
				t.Fatalf("move: %v", err)
			}
			moveStep = func(string) error { return nil }

			// The app starts again: the registry still says moving.
			pending, err := state.OpenRegistry(base).Get(conn.ID)
			if err != nil || pending.MovingTo != to {
				t.Fatalf("after the crash: %+v, %v", pending, err)
			}
			s, err := Open(ctx, signin.NewFileStore(base), pending, Options{Base: base})
			if err != nil {
				t.Fatal(err)
			}
			if s.Conn.LocalRoot != to {
				t.Errorf("opened at %s", s.Conn.LocalRoot)
			}
			s.Close()
			expectAt(t, base, conn, to)
		})
	}
}

// Undone instead when the new place got files before the rename, and
// left pending (the engine not started) when the folder is in neither
// place.
func TestMoveFolderCrashUndo(t *testing.T) {
	ctx := context.Background()
	home, base := testBase(t)
	conn := setup(t, base, "u1", filepath.Join(home, "Cloud", "a"))
	write(t, filepath.Join(conn.LocalRoot, "Docs", "a.txt"), "a")
	withPart(t, conn)
	to := filepath.Join(home, "Picked")
	crash(t, "begun")
	if _, err := MoveFolder(ctx, base, conn, to); !errors.Is(err, errCrash) {
		t.Fatalf("move: %v", err)
	}
	moveStep = func(string) error { return nil }
	write(t, filepath.Join(to, "someone's.txt"), "s")
	pending, _ := state.OpenRegistry(base).Get(conn.ID)
	s, err := Open(ctx, signin.NewFileStore(base), pending, Options{Base: base})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	expectAt(t, base, conn, conn.LocalRoot)
	if b, err := os.ReadFile(filepath.Join(to, "someone's.txt")); err != nil || string(b) != "s" {
		t.Fatalf("the other folder's file: %q, %v", b, err)
	}

	// The folder unplugged mid-move: nothing to tell where it is.
	crash(t, "begun")
	if _, err := MoveFolder(ctx, base, conn, filepath.Join(home, "Other")); !errors.Is(err, errCrash) {
		t.Fatalf("move: %v", err)
	}
	moveStep = func(string) error { return nil }
	away := filepath.Join(home, "unplugged")
	if err := os.Rename(conn.LocalRoot, away); err != nil {
		t.Fatal(err)
	}
	pending, _ = state.OpenRegistry(base).Get(conn.ID)
	if _, err := Open(ctx, signin.NewFileStore(base), pending, Options{Base: base}); !errors.Is(err, localfs.ErrNoMarker) {
		t.Fatalf("open with the folder in neither place: %v", err)
	}
	if got, _ := state.OpenRegistry(base).Get(conn.ID); got.MovingTo == "" {
		t.Fatalf("the move was forgotten: %+v", got)
	}
	// Plugged in again: the move goes on.
	if err := os.Rename(away, conn.LocalRoot); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, signin.NewFileStore(base), pending, Options{Base: base})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	expectAt(t, base, conn, filepath.Join(home, "Other"))
}

var errCrash = errors.New("crash")

// crash makes the next move stop after step, as if the process died.
func crash(t *testing.T, step string) {
	moveStep = func(s string) error {
		if s == step {
			return fmt.Errorf("%w after %s", errCrash, s)
		}
		return nil
	}
	t.Cleanup(func() { moveStep = func(string) error { return nil } })
}
