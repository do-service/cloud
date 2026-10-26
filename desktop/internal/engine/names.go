package engine

import (
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Paths here are the state's: relative to the sync folder, slash-separated,
// "" for the folder itself.

func dirOf(p string) string {
	d := path.Dir(p)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

func nameOf(p string) string { return path.Base(p) }

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// under reports whether p is dir or inside it; everything is under "".
func under(p, dir string) bool {
	return dir == "" || p == dir || strings.HasPrefix(p, dir+"/")
}

// below: strictly inside dir.
func below(p, dir string) bool { return p != dir && under(p, dir) }

func nfc(s string) string { return norm.NFC.String(s) }

// conflictName is «Имя (конфликт, Устройство, ГГГГ-ММ-ДД ЧЧ-ММ).ext»
// (docs/desktop-client.md, «Конфликты»), with n > 1 appended inside the
// parentheses when that name is taken. The extension is the last one; a
// dotfile has none. The stem is shortened to keep the name within the
// server's 255 bytes.
func conflictName(name, device string, at time.Time, n int) string {
	return taggedName(name, "конфликт", device, at, n)
}

// tempName is «Имя (перенос, Устройство, ГГГГ-ММ-ДД ЧЧ-ММ).ext»: where a
// node waits while a cycle of renames (a and b swapped) goes through, on
// the server or on disk. It lasts a pass or two.
func tempName(name, device string, at time.Time, n int) string {
	return taggedName(name, "перенос", device, at, n)
}

// isTempName: name is a tempName of this device.
func isTempName(name, device string) bool {
	name = nfc(name)
	if i := strings.LastIndexByte(name, '.'); i > 0 && strings.HasSuffix(name[:i], ")") {
		name = name[:i]
	}
	tag := " (перенос, " + cleanDevice(device) + ", "
	i := strings.LastIndex(name, tag)
	if i < 0 || !strings.HasSuffix(name, ")") {
		return false
	}
	rest := name[i+len(tag) : len(name)-1]
	stamp, n, _ := strings.Cut(rest, ", ")
	if _, err := time.Parse("2006-01-02 15-04", stamp); err != nil {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func taggedName(name, label, device string, at time.Time, n int) string {
	name = nfc(name)
	stem, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		stem, ext = name[:i], name[i:]
	}
	tag := fmt.Sprintf(" (%s, %s, %s", label, cleanDevice(device), at.Format("2006-01-02 15-04"))
	if n > 1 {
		tag += fmt.Sprintf(", %d", n)
	}
	tag += ")"
	if room := 255 - len(tag) - len(ext); len(stem) > room {
		stem = truncateUTF8(stem, max(room, 1))
	}
	return stem + tag + ext
}

// cleanDevice keeps a device name usable inside a file name on every OS.
func cleanDevice(d string) string {
	d = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '-'
		}
		return r
	}, strings.TrimSpace(d))
	if d == "" {
		d = "компьютер"
	}
	return truncateUTF8(d, 64)
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
