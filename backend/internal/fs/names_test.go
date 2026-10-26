package fs

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeName(t *testing.T) {
	cases := []struct {
		in, name, key string
		bad           bool
	}{
		{in: "Отчёт за 2026.docx", name: "Отчёт за 2026.docx", key: "отчёт за 2026.docx"},
		{in: "  spaces and !@#$%^&()[]{}  ", name: "  spaces and !@#$%^&()[]{}  ", key: "  spaces and !@#$%^&()[]{}  "},
		{in: `win<>:"\|?*`, name: `win<>:"\|?*`, key: `win<>:"\|?*`},
		{in: "emoji 🎉", name: "emoji 🎉", key: "emoji 🎉"},
		// NFD (as macOS sends it) becomes NFC.
		{in: "Café", name: "Café", key: "café"},
		{in: "..hidden", name: "..hidden", key: "..hidden"},
		{in: strings.Repeat("я", 127), name: strings.Repeat("я", 127), key: strings.Repeat("я", 127)}, // 254 bytes
		{in: "", bad: true},
		{in: ".", bad: true},
		{in: "..", bad: true},
		{in: "a/b", bad: true},
		{in: "a\x00b", bad: true},
		{in: "line\nbreak", bad: true},
		{in: "tab\t", bad: true},
		{in: "\xff\xfe", bad: true},
		{in: strings.Repeat("я", 128), bad: true}, // 256 bytes
	}
	for _, tc := range cases {
		name, key, err := NormalizeName(tc.in)
		if tc.bad {
			if !errors.Is(err, ErrInvalidName) {
				t.Errorf("%q: err %v, want ErrInvalidName", tc.in, err)
			}
			continue
		}
		if err != nil || name != tc.name || key != tc.key {
			t.Errorf("%q: (%q, %q, %v), want (%q, %q)", tc.in, name, key, err, tc.name, tc.key)
		}
	}
}
