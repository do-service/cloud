// Package connection is a connection's life outside the engine, shared by
// cloudsync and the app (docs/desktop-client.md, «Подключения»): setting
// it up after a sign-in (registry entry, sync folder with its marker,
// state.db), opening it for sync under its lock, and removing it.
package connection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/engine"
	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/state"
)

// ErrOtherFolder: the account is connected already, to another folder
// than the one asked for.
var ErrOtherFolder = errors.New("connection: this account is connected to another folder")

// DefaultFolder is ~/Cloud/<server host>, the port joined with "-" so two
// servers on one host get two folders.
func DefaultFolder(server string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	name := u.Hostname()
	if p := u.Port(); p != "" {
		name += "-" + p
	}
	return filepath.Join(home, "Cloud", name), nil
}

// Setup makes a signed-in account a connection: its entry in the
// registry (an existing one of the same account is reused), the sync
// folder with its marker, and state.db with the account. dir "" is the
// default folder; for an existing connection it must match or be ""
// (ErrOtherFolder). A new connection's folder is checked (CheckFolder);
// what it already holds is adopted, not uploaded: its top-level names go
// to state.db (Tx.SetAdopted) before the marker lets the engine run, so
// the first pass pairs them with the cloud where the same thing is
// checked, and leaves the rest alone. It reports whether the connection
// existed.
func Setup(ctx context.Context, base string, a signin.Account, dir string) (state.Connection, bool, error) {
	reg := state.OpenRegistry(base)
	conns, err := reg.List()
	if err != nil {
		return state.Connection{}, false, err
	}
	var conn state.Connection
	existing := false
	for _, c := range conns {
		if c.Server == a.Server && c.UserID == a.UserID {
			conn, existing = c, true
		}
	}
	if dir != "" {
		if dir, err = filepath.Abs(dir); err != nil {
			return state.Connection{}, false, err
		}
	}
	var folder Folder
	var adopt []string
	switch {
	case existing && dir != "" && filepath.Clean(dir) != filepath.Clean(conn.LocalRoot):
		return conn, true, fmt.Errorf("%w: %s", ErrOtherFolder, conn.LocalRoot)
	case !existing:
		if dir == "" {
			if dir, err = DefaultFolder(a.Server); err != nil {
				return state.Connection{}, false, err
			}
		}
		if folder, err = CheckFolder(base, a.Server, a.UserID, dir); err != nil {
			return state.Connection{}, false, err
		}
		if adopt, err = topLevel(folder.Path); err != nil {
			return state.Connection{}, false, err
		}
		if conn, err = reg.Add(state.Connection{Server: a.Server, UserID: a.UserID, UserName: a.Username, LocalRoot: folder.Path}); err != nil {
			return state.Connection{}, false, err
		}
	}
	if err := os.MkdirAll(conn.LocalRoot, 0o755); err != nil {
		return conn, existing, err
	}
	path, err := state.Path(conn.ID)
	if err != nil {
		return conn, existing, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return conn, existing, err
	}
	db, err := state.Open(ctx, path)
	if err != nil {
		return conn, existing, err
	}
	err = db.Update(ctx, func(tx *state.Tx) error {
		acc, err := tx.Account()
		if errors.Is(err, state.ErrNotFound) {
			if existing {
				// Registered by a Setup that crashed before state.db: the
				// folder is as it found it.
				if adopt, err = topLevel(conn.LocalRoot); err != nil {
					return err
				}
			}
			if err := tx.SetAdopted(adopt); err != nil {
				return err
			}
			return tx.SetAccount(state.Account{Server: conn.Server, UserID: conn.UserID, DeviceID: a.DeviceID, LocalRoot: conn.LocalRoot})
		}
		if err != nil {
			return err
		}
		// Signed in again: a new device, the same folder and state.
		acc.DeviceID = a.DeviceID
		return tx.SetAccount(acc)
	})
	db.Close()
	if err != nil {
		return conn, existing, err
	}
	if existing {
		err = localfs.CreateMarker(conn.LocalRoot, conn.ID)
	} else {
		err = localfs.WriteMarker(conn.LocalRoot, localfs.Marker{ConnID: conn.ID, Server: conn.Server, UserID: conn.UserID}, folder.Replace)
	}
	return conn, existing, err
}

// Session is an open connection: held under its lock (state.LockConnection),
// with its database, server client and engine.
type Session struct {
	Conn   state.Connection
	DB     *state.DB
	Client *api.Client
	Engine *engine.Engine
	lock   *state.Lock
}

// Close closes the database and releases the lock. Stop the engine
// first (cancel Run and let it return).
func (s *Session) Close() {
	s.DB.Close()
	_ = s.lock.Unlock()
}

type Options struct {
	// Base is the state directory with the registry (state.BaseDir()):
	// needed to finish a folder move a crash interrupted (MoveFolder).
	Base           string
	ConfirmDeletes bool
	// UpLimit and DownLimit in bytes per second; 0 is none.
	UpLimit, DownLimit int
	Tune               func(*engine.Config)
	Logger             *slog.Logger
}

// Open takes conn's lock (state.ErrLocked if another process has it) and
// opens its engine with the account's token from st. A folder move left
// unfinished is finished (or undone) first; Session.Conn has the folder
// as it then is.
func Open(ctx context.Context, st *signin.Store, conn state.Connection, opts Options) (*Session, error) {
	lock, err := state.LockConnection(conn.ID)
	if err != nil {
		return nil, err
	}
	if conn.MovingTo != "" {
		if opts.Base == "" {
			_ = lock.Unlock()
			return nil, errors.New("connection: a folder move to finish, and no state directory given")
		}
		c, err := finishMove(ctx, opts.Base, conn)
		if c.MovingTo != "" {
			_ = lock.Unlock()
			return nil, err
		}
		if err != nil {
			// Undone: syncs where it was.
			slog.Warn("connection: the sync folder's move was undone", "conn", conn.ID, "err", err)
		}
		conn = c
	}
	s, err := open(ctx, st, conn, opts)
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	s.lock = lock
	return s, nil
}

func open(ctx context.Context, st *signin.Store, conn state.Connection, opts Options) (*Session, error) {
	path, err := state.Path(conn.ID)
	if err != nil {
		return nil, err
	}
	db, err := state.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	device := ""
	if accounts, err := st.Accounts(); err == nil {
		for _, a := range accounts {
			if a.Server == conn.Server && a.UserID == conn.UserID {
				device = a.DeviceName
			}
		}
	}
	if device == "" {
		device = signin.DeviceName()
	}
	client, err := api.New(conn.Server, api.Options{Token: st.TokenSource(conn.Server, conn.UserID),
		UploadLimit: api.NewLimiter(opts.UpLimit), DownloadLimit: api.NewLimiter(opts.DownLimit)})
	if err != nil {
		db.Close()
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	cfg := engine.Config{
		DB: db, Remote: client, Root: conn.LocalRoot, ConnID: conn.ID, UserID: conn.UserID,
		Options: engine.DefaultOptions(device), ConfirmDeletes: opts.ConfirmDeletes, Logger: logger,
	}
	if opts.Tune != nil {
		opts.Tune(&cfg)
	}
	e, err := engine.New(cfg)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Session{Conn: conn, DB: db, Client: client, Engine: e}, nil
}

// Remove disconnects conn: signs its device out on the server (unless
// force and the server can't be reached; see signin.LogoutOptions),
// forgets the token, the registry entry and the state directory, and
// takes the marker out of the sync folder. The files in the folder stay:
// they are the user's, now a plain folder. It takes the connection's
// lock, so the caller's own engine must be stopped (Session closed).
func Remove(ctx context.Context, base string, st *signin.Store, conn state.Connection, force bool) error {
	lock, err := state.LockConnection(conn.ID)
	if err != nil {
		return err
	}
	if err := st.Logout(ctx, conn.Server, conn.UserID, signin.LogoutOptions{Force: force}); err != nil {
		_ = lock.Unlock()
		return err
	}
	if err := state.OpenRegistry(base).Remove(conn.ID); err != nil && !errors.Is(err, state.ErrNotFound) {
		_ = lock.Unlock()
		return err
	}
	// Only our own marker: a folder another connection took since is
	// left alone.
	// Either place, if a move was left unfinished.
	for _, root := range []string{conn.LocalRoot, conn.MovingTo} {
		if root != "" && localfs.CheckMarker(root, conn.ID) == nil {
			if err := os.Remove(filepath.Join(root, localfs.MarkerName)); err != nil {
				slog.Warn("connection: remove the sync folder's marker", "folder", root, "err", err)
			}
		}
	}
	dir, err := state.Dir(conn.ID)
	_ = lock.Unlock()
	if err != nil {
		return err
	}
	// After the registry: a crash in between leaves a directory nothing
	// lists, not a connection without its state.
	return os.RemoveAll(dir)
}
