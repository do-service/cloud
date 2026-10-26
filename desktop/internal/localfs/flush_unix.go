//go:build unix && !darwin

package localfs

import (
	"errors"
	"os"
)

// fileSync is fsync: on Linux it is durable, so the data is on stable
// storage before the rename that follows and no barrier is needed.
func fileSync(f *os.File) error { return f.Sync() }

func barrierDirs([]string) error { return nil }

// flushDirs fsyncs each folder once: Linux makes a rename durable by the
// folder's fsync.
func flushDirs(dirs []string) error {
	for _, d := range dirs {
		// Gone since: whatever moved or removed it flushed its parent.
		if err := syncDir(d); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
