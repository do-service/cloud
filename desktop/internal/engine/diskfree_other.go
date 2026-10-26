//go:build !unix && !windows

package engine

// freeSpace isn't known here: the check is skipped.
func freeSpace(string) (int64, bool) { return 0, false }
