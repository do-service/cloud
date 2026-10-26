package localfs

import "golang.org/x/sys/unix"

// birthTime is path's creation time (statx btime) in Unix nanoseconds:
// ext4, btrfs, xfs and tmpfs keep it, and it never changes for a file,
// renames included. 0 when the file system doesn't report it, or the
// entry at path isn't inode ino any more (replaced since the stat).
func birthTime(path string, ino uint64) int64 {
	var stx unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_DONT_SYNC,
		unix.STATX_INO|unix.STATX_BTIME, &stx)
	if err != nil || stx.Mask&unix.STATX_BTIME == 0 || stx.Mask&unix.STATX_INO == 0 || stx.Ino != ino {
		return 0
	}
	return stx.Btime.Sec*1e9 + int64(stx.Btime.Nsec)
}
