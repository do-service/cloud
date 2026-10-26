package localfs

import (
	"os"

	"golang.org/x/sys/unix"
)

// fileSync is a plain fsync(2): the data goes to the device, which may
// still hold it in its cache or reorder it; barrierDirs orders it.
func fileSync(f *os.File) error {
	if err := unix.Fsync(int(f.Fd())); err != nil {
		return &os.PathError{Op: "fsync", Path: f.Name(), Err: err}
	}
	return nil
}

// barrierDirs issues one F_BARRIERFSYNC per device of dirs: what was
// fsynced on the device before reaches stable storage before anything
// written after, a rename included. A file system without it (SMB, some
// FUSE ones) gets F_FULLFSYNC, which Go's Sync is on macOS.
func barrierDirs(dirs []string) error {
	return perDevice(dirs, func(f *os.File) error {
		if _, err := unix.FcntlInt(f.Fd(), unix.F_BARRIERFSYNC, 0); err == nil {
			return nil
		}
		return f.Sync()
	})
}

// flushDirs moves each folder's entries to its device (fsync), then
// drains each device's cache once (F_FULLFSYNC): everything fsynced
// before it is then persisted, the files' data included.
func flushDirs(dirs []string) error {
	for _, d := range dirs {
		f, err := os.Open(d)
		if dirGone(err) {
			// Renamed or removed since: whatever moved it flushes its
			// parent, and the device is drained for the others anyway.
			continue
		}
		if err != nil {
			return err
		}
		err = fileSync(f)
		f.Close()
		if err != nil {
			return err
		}
	}
	return perDevice(dirs, (*os.File).Sync)
}

// perDevice calls fn on one of dirs of each device.
func perDevice(dirs []string, fn func(*os.File) error) error {
	seen := map[int32]bool{}
	for _, d := range dirs {
		f, err := os.Open(d)
		if dirGone(err) {
			continue
		}
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &st); err != nil {
			f.Close()
			return &os.PathError{Op: "fstat", Path: d, Err: err}
		}
		if !seen[st.Dev] {
			seen[st.Dev] = true
			err = fn(f)
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
