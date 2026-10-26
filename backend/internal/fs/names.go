package fs

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// maxNameBytes is the per-component limit of ext4, XFS and Btrfs: a longer
// name could be stored in the database but never written to disk.
const maxNameBytes = 255

// NormalizeName validates a file or folder name and returns it in NFC
// along with its name_key.
//
// NFC because macOS clients send decomposed (NFD) names and Windows ones
// composed: without normalizing, the same visible name would be two
// different files on disk while sharing one name_key. The key is the
// lower-cased NFC form, so names differing only in case conflict, as they
// would on the case-insensitive filesystems of WebDAV clients (risk A4).
//
// Rejected: empty, "." and "..", "/", invalid UTF-8, names over 255 bytes,
// and control characters U+0000–U+001F — XML 1.0 can't represent most of
// them, so a WebDAV listing containing one would be unparseable. Characters
// Windows forbids (<>:"\|?*) are allowed, as in Nextcloud.
func NormalizeName(name string) (normalized, key string, err error) {
	if !utf8.ValidString(name) {
		return "", "", ErrInvalidName
	}
	n := norm.NFC.String(name)
	if n == "" || n == "." || n == ".." || len(n) > maxNameBytes {
		return "", "", ErrInvalidName
	}
	for _, r := range n {
		if r < 0x20 || r == '/' {
			return "", "", ErrInvalidName
		}
	}
	return n, strings.ToLower(n), nil
}
