//go:build windows

package localfs

import (
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// fileID opens the file to ask for its index: Windows' stat from a
// directory listing doesn't carry it. Opened without access rights and
// with every share mode, so it works on files another program has open,
// and with OPEN_REPARSE_POINT, so a link is not followed.
func fileID(path string, _ fs.FileInfo) (FileID, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return FileID{}, err
	}
	h, err := windows.CreateFile(p, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return FileID{}, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return FileID{}, &fs.PathError{Op: "GetFileInformationByHandle", Path: path, Err: err}
	}
	return FileID{
		Volume: uint64(info.VolumeSerialNumber),
		Index:  uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}

// syncDir is a no-op: Windows can't open a directory for flushing, and
// NTFS journals the rename itself.
func syncDir(string) error { return nil }

// fileSync is FlushFileBuffers: Windows has nothing lighter, and nothing
// to order a rename after it with.
func fileSync(f *os.File) error { return f.Sync() }

func barrierDirs([]string) error { return nil }

// flushDirs is a no-op, as syncDir is.
func flushDirs([]string) error { return nil }
