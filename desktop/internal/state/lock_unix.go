//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile: flock, not fcntl locks, which any close of the file in the
// process drops (state.db's SQLite opens and closes files freely).
func lockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return ErrLocked
	}
	if err != nil {
		return fmt.Errorf("state: lock: %w", err)
	}
	return nil
}
