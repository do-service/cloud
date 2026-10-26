//go:build unix

package engine

import "golang.org/x/sys/unix"

// freeSpace is what the user may still write on the volume of dir, in
// bytes; ok false where it can't be told.
func freeSpace(dir string) (free int64, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
