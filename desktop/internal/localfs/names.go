package localfs

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// ErrInvalidName is a name the server refuses.
var ErrInvalidName = errors.New("localfs: invalid name")

// maxNameBytes is the server's limit (backend/internal/fs/names.go).
const maxNameBytes = 255

// NormalizeName is the server's fs.NormalizeName, ported: the name in NFC
// and its name_key, or ErrInvalidName for what the server would refuse
// (empty, "." and "..", "/", invalid UTF-8, over 255 bytes after NFC,
// control characters below U+0020). Both sides check themselves against
// backend/internal/fs/testdata/name_vectors.json; change them together.
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

// NameKey is how the server compares names: lower-cased NFC. Two names
// with one key are one name to the server, and to a case-insensitive
// disk (macOS and Windows by default). Defined for invalid names too, so
// a scan can still group them.
func NameKey(name string) string {
	return strings.ToLower(norm.NFC.String(name))
}

// Collisions groups names that share a NameKey, in the order given; only
// groups of two or more are returned. On a case-sensitive disk (Linux)
// "a.txt" and "A.txt" are two files the server can't hold side by side.
func Collisions(names []string) [][]string {
	groups := map[string][]string{}
	var order []string
	for _, n := range names {
		k := NameKey(n)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], n)
	}
	var out [][]string
	for _, k := range order {
		if len(groups[k]) > 1 {
			out = append(out, groups[k])
		}
	}
	return out
}

// ErrWindowsName is a name Windows can't store; such files aren't
// downloaded there and show in «Проблемы».
var ErrWindowsName = errors.New("localfs: name not allowed on Windows")

// CheckWindowsName reports why Windows would refuse name: one of
// <>:"/\|?* or a control character, a trailing dot or space (Explorer
// silently strips them), or a reserved device name, with or without an
// extension (CON, NUL.txt, COM1.tar.gz). The server allows all of them,
// as Nextcloud does.
func CheckWindowsName(name string) error {
	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return fmt.Errorf("%w: %q contains %q", ErrWindowsName, name, r)
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("%w: %q ends with a dot or space", ErrWindowsName, name)
	}
	stem, _, _ := strings.Cut(name, ".")
	if reservedWindows(strings.TrimRight(stem, " ")) {
		return fmt.Errorf("%w: %q is a reserved device name", ErrWindowsName, name)
	}
	return nil
}

func reservedWindows(stem string) bool {
	switch strings.ToUpper(stem) {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(stem) < 4 {
		return false
	}
	prefix := strings.ToUpper(stem[:3])
	if prefix != "COM" && prefix != "LPT" {
		return false
	}
	// COM0–COM9, LPT0–LPT9 and the superscript digits ¹²³, which Windows
	// also treats as digits here.
	switch stem[3:] {
	case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
		return true
	}
	return false
}
