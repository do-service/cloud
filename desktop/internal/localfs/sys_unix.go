//go:build unix

package localfs

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// fileID reads the id from the stat the caller already has: free on Unix.
func fileID(_ string, fi fs.FileInfo) (FileID, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return FileID{}, fmt.Errorf("localfs: no stat_t for %s", fi.Name())
	}
	// Dev is int32 on darwin and uint64 on linux.
	return FileID{Volume: uint64(st.Dev), Index: uint64(st.Ino)}, nil
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
