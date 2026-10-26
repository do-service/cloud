package localfs

import "strings"

const (
	// MarkerName is the file at the root of a sync folder that says it is
	// one (marker.go).
	MarkerName = ".cloudsync"
	// tempPrefix starts the names of this client's own temporary files:
	// downloads in progress (.~cloud-<id>.part) and case-only renames.
	tempPrefix = ".~cloud-"
)

// Ignored reports whether a file or folder with this name is left out of
// sync, wherever it is: the marker, this client's temporary files, OS
// metadata, and the temporary and lock files editors and browsers keep
// next to a document while it is open or downloading. Matching is by
// name only, so a folder named like this is skipped with all it holds.
func Ignored(name string) bool {
	if name == MarkerName || strings.HasPrefix(name, tempPrefix) || junk(name) {
		return true
	}
	lower := strings.ToLower(name)
	switch {
	case strings.HasPrefix(name, "~$"): // Word, Excel: owner file of an open document
		return true
	case strings.HasPrefix(name, ".~lock.") && strings.HasSuffix(name, "#"): // LibreOffice
		return true
	}
	for _, ext := range []string{".tmp", ".crdownload", ".part"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// junk is the OS's metadata, which appears in any folder a file manager
// opened: ignored, and safe to delete with the folder (Remove), unlike a
// lock file, which means a document is still open.
func junk(name string) bool {
	switch {
	case name == ".DS_Store", name == "Icon\r":
		return true
	case strings.EqualFold(name, "Thumbs.db"), strings.EqualFold(name, "desktop.ini"):
		return true
	}
	return false
}
