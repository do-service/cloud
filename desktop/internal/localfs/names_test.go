package localfs

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

// The golden vectors the server's NormalizeName is checked against
// (backend/internal/fs/names_vectors_test.go). One file for both sides,
// so a change to either one's rules fails a test until both agree.
const vectorsPath = "../../../backend/internal/fs/testdata/name_vectors.json"

func TestNameVectors(t *testing.T) {
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Note, In, InHex, Name, Key string
		Valid                      bool
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) < 30 {
		t.Fatalf("only %d vectors", len(vectors))
	}
	for _, v := range vectors {
		in := v.In
		if v.InHex != "" {
			b, err := hex.DecodeString(v.InHex)
			if err != nil {
				t.Fatal(err)
			}
			in = string(b)
		}
		name, key, err := NormalizeName(in)
		if v.Valid != (err == nil) || name != v.Name || key != v.Key {
			t.Errorf("%s: NormalizeName(%q) = (%q, %q, %v), server: (%q, %q, valid %v)", v.Note, in, name, key, err, v.Name, v.Key, v.Valid)
		}
		if err != nil && !errors.Is(err, ErrInvalidName) {
			t.Errorf("%s: %v", v.Note, err)
		}
		if v.Valid && NameKey(in) != v.Key {
			t.Errorf("%s: NameKey(%q) = %q, server: %q", v.Note, in, NameKey(in), v.Key)
		}
	}
}

func TestCheckWindowsName(t *testing.T) {
	for _, ok := range []string{"report.txt", "Отчёт.docx", "CONSOLE", "con1", "COM10", "LPT", ".hidden", "a.b.c", "NULL.txt", "x . y", "COMx"} {
		if err := CheckWindowsName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"CON", "con", "Nul.txt", "AUX.tar.gz", "PRN ", "COM1", "lpt9.doc", "COM0", "COM¹", "LPT³.txt", "CONIN$", "NUL .txt",
		"a<b", "a>b", "a:b", `a"b`, `a\b`, "a|b", "a?b", "a*b", "a\x01b",
		"trailing.", "trailing ",
	} {
		if err := CheckWindowsName(bad); !errors.Is(err, ErrWindowsName) {
			t.Errorf("%q: %v, want ErrWindowsName", bad, err)
		}
	}
}

func TestCollisions(t *testing.T) {
	got := Collisions([]string{"a.txt", "B", "A.TXT", "c", "b", "Café", "CAFÉ", "d"})
	want := [][]string{{"a.txt", "A.TXT"}, {"B", "b"}, {"Café", "CAFÉ"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Collisions = %q, want %q", got, want)
	}
	if got := Collisions([]string{"x", "y"}); got != nil {
		t.Fatalf("Collisions without any = %q", got)
	}
}

func TestIgnored(t *testing.T) {
	for _, name := range []string{
		".DS_Store", "Icon\r", "Thumbs.db", "thumbs.DB", "desktop.ini", "Desktop.ini",
		"~$Отчёт.docx", ".~lock.Отчёт.odt#", "data.tmp", "DATA.TMP", "file.crdownload", "big.iso.part",
		".~cloud-0f9a.part", ".~cloud-rename-1234", MarkerName,
	} {
		if !Ignored(name) {
			t.Errorf("%q not ignored", name)
		}
	}
	for _, name := range []string{
		"report.txt", ".DS_Store2", "Icon", ".~lock.file", "tmp", "party.txt", "~file", ".cloudsync2", "part", "a.tmp.txt",
	} {
		if Ignored(name) {
			t.Errorf("%q ignored", name)
		}
	}
}

func TestFileIDString(t *testing.T) {
	for _, id := range []FileID{{}, {Volume: 1, Index: 2}, {Volume: ^uint64(0), Index: 1 << 63}} {
		got, err := ParseFileID(id.String())
		if err != nil || got != id {
			t.Errorf("ParseFileID(%q) = %v, %v", id.String(), got, err)
		}
	}
	for _, bad := range []string{"x", "1:", ":1", "g:1"} {
		if _, err := ParseFileID(bad); err == nil {
			t.Errorf("ParseFileID(%q) accepted", bad)
		}
	}
}
