//go:build unix

package localfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// fileID reads the id from the stat the caller already has, and on Linux
// the birth time (birthTime) from one more.
func fileID(path string, fi fs.FileInfo) (FileID, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return FileID{}, fmt.Errorf("localfs: no stat_t for %s", fi.Name())
	}
	// Dev is int32 on darwin and uint64 on linux.
	return FileID{Volume: uint64(st.Dev), Index: uint64(st.Ino), Birth: birthTime(path, uint64(st.Ino))}, nil
}

// dirGone: a folder queued for a flush isn't there any more — removed,
// or a folder on its path replaced by a file (ENOTDIR). Whatever did that
// flushed the parent.
func dirGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// syncDir makes a rename or delete in dir durable: without it a power cut
// can bring the old name back although the rename returned.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
