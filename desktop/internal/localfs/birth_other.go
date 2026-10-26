//go:build unix && !linux

package localfs

// birthTime is 0: APFS and HFS+ don't hand a deleted file's inode number
// to a new one (APFS numbers objects from a counter that only grows; HFS+
// reuses catalog ids only after 2^32 of them), so device and inode tell
// files apart. The birth time there can also be set by programs, which
// would make a file look replaced.
func birthTime(string, uint64) int64 { return 0 }
