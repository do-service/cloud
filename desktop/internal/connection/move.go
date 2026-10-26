package connection

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// moveStep is called after each step of a move; tests make it fail to
// stop a move there, as a crash would.
var moveStep = func(string) error { return nil }

// MoveFolder moves conn's sync folder to dir («Сменить папку»): dir must
// be a new or empty folder (CheckMoveTarget) on the same disk. The
// caller's engine must be stopped (Session closed): it takes the lock.
//
// Only a rename: a move to another disk would be a copy of everything,
// then a check, then a delete of the original — hours for a big folder,
// with twice the space, and a crash or an unplugged disk midway leaving
// two half-trees to tell apart. It is refused (ErrCrossVolume); the
// user can move the files themselves and connect the folder again.
//
// Crash-safe: the intent goes to the registry first (MovingTo), then the
// folder is renamed in one step, then state.db and the registry follow.
// Whichever step a crash interrupts, the next Open finishes the move or,
// if the folder is still where it was and the new place can't take it,
// undoes it: the marker, which moves with the folder, tells where it is.
func MoveFolder(ctx context.Context, base string, conn state.Connection, dir string) (state.Connection, error) {
	f, err := CheckMoveTarget(base, conn, dir)
	if err != nil {
		return conn, err
	}
	lock, err := state.LockConnection(conn.ID)
	if err != nil {
		return conn, err
	}
	defer lock.Unlock()
	if err := localfs.CheckMarker(conn.LocalRoot, conn.ID); err != nil {
		// Not where it should be: nothing to move.
		return conn, err
	}
	c, err := state.OpenRegistry(base).BeginMove(conn.ID, f.Path)
	if err != nil {
		return conn, err
	}
	if err := moveStep("begun"); err != nil {
		return c, err
	}
	return finishMove(ctx, base, c)
}

// finishMove carries out the move c.MovingTo records, under c's lock.
// It returns the connection as the registry has it after: moved, or
// still where it was (MovingTo "") with the error that undid the move,
// or still moving with the error that stopped it (the folder in neither
// place: a disk unplugged — tried again on the next Open).
func finishMove(ctx context.Context, base string, c state.Connection) (state.Connection, error) {
	reg := state.OpenRegistry(base)
	from, to := c.LocalRoot, c.MovingTo
	switch {
	case localfs.CheckMarker(to, c.ID) == nil:
		// Renamed before a crash.
	case localfs.CheckMarker(from, c.ID) == nil:
		existed, err := clearTarget(to)
		if err != nil {
			return undoMove(reg, c, err)
		}
		if err := moveStep("cleared"); err != nil {
			return c, err
		}
		if err := os.Rename(from, to); err != nil {
			if existed {
				_ = os.Mkdir(to, 0o755)
			}
			fv, ferr := localfs.VolumeOf(from)
			tv, terr := localfs.VolumeOf(filepath.Dir(to))
			if ferr == nil && terr == nil && fv != tv {
				err = fmt.Errorf("%w: %s", ErrCrossVolume, to)
			}
			return undoMove(reg, c, err)
		}
		syncDir(filepath.Dir(to))
		syncDir(filepath.Dir(from))
	default:
		return c, fmt.Errorf("%w: neither %s nor %s", localfs.ErrNoMarker, from, to)
	}
	if err := moveStep("renamed"); err != nil {
		return c, err
	}
	path, err := state.Path(c.ID)
	if err != nil {
		return c, err
	}
	db, err := state.Open(ctx, path)
	if err != nil {
		return c, err
	}
	err = db.Update(ctx, func(tx *state.Tx) error { return tx.MoveRoot(to) })
	db.Close()
	if err != nil {
		return c, err
	}
	if err := moveStep("recorded"); err != nil {
		return c, err
	}
	return reg.EndMove(c.ID, true)
}

func undoMove(reg *state.Registry, c state.Connection, cause error) (state.Connection, error) {
	back, err := reg.EndMove(c.ID, false)
	if err != nil {
		return c, errors.Join(cause, err)
	}
	return back, cause
}

// clearTarget makes room for the rename: the parent folders, and no
// folder at to — an empty one the user picked (Finder may have left a
// .DS_Store in it) is removed, the sync folder takes its place. Anything
// else there now (written since the check) undoes the move.
func clearTarget(to string) (existed bool, err error) {
	ents, err := os.ReadDir(to)
	if errors.Is(err, fs.ErrNotExist) {
		return false, os.MkdirAll(filepath.Dir(to), 0o755)
	}
	if err != nil {
		return false, err
	}
	for _, e := range ents {
		if e.Name() != ".DS_Store" {
			return false, fmt.Errorf("%w: %s", ErrFolderNotEmpty, to)
		}
	}
	if err := os.Remove(filepath.Join(to, ".DS_Store")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, os.Remove(to)
}

// syncDir makes the rename durable where the OS allows (not on Windows,
// whose NTFS journals it): a power cut can otherwise bring the old name
// back after the registry moved on, and the engine would find no marker
// where the registry says and stop.
func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		_ = f.Sync()
		f.Close()
	}
}
