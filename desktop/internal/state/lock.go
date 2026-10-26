package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LockFile is the lock's name in a connection's directory.
const LockFile = "lock"

// ErrLocked: another process (the app, or cloudsync) has the connection.
var ErrLocked = errors.New("state: the connection is in use by another process")

// Lock is a connection held by this process: two engines on one sync
// folder and state.db would each plan against the other's half-done
// work. It is an OS lock on a file of the connection's directory, so a
// crashed holder releases it with its last descriptor; nothing to clean.
type Lock struct{ f *os.File }

// LockConnection takes connID's lock without waiting: ErrLocked if it is
// held. It creates the connection's directory if needed.
func LockConnection(connID string) (*Lock, error) {
	dir, err := Dir(connID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, LockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state: lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Unlock releases the lock; closing the file does.
func (l *Lock) Unlock() error { return l.f.Close() }
