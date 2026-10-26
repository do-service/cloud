//go:build !unix

package engine

// freeSpace isn't known here (Windows until E15-2): the check is skipped.
func freeSpace(string) (int64, bool) { return 0, false }
