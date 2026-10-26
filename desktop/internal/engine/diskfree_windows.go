package engine

import "golang.org/x/sys/windows"

// freeSpace is what the user may still write on the volume of dir, in
// bytes (disk quotas included); ok false where it can't be told.
func freeSpace(dir string) (free int64, ok bool) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var avail uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, nil, nil); err != nil {
		return 0, false
	}
	return int64(avail), true
}
