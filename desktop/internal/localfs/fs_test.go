package localfs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func paths(r Result) []string {
	var out []string
	for _, e := range r.Entries {
		out = append(out, e.RelPath)
	}
	return out
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.txt", "hello")
	write(t, root, "Папка/b.txt", "bb")
	write(t, root, "Папка/sub/c.txt", "c")
	write(t, root, "Папка/.DS_Store", "junk")
	write(t, root, "Папка/~$b.docx", "lock")
	write(t, root, "big.iso.part", "x")
	write(t, root, ".~cloud-n1.part", "x")
	write(t, root, MarkerName, "conn")
	write(t, root, "ignored.tmp/inside.txt", "x")
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "Папка"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	res, err := Scan(root, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.txt", "empty", "Папка", "Папка/b.txt", "Папка/sub", "Папка/sub/c.txt"}
	if !slices.Equal(paths(res), want) {
		t.Fatalf("Scan = %q, want %q", paths(res), want)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].RelPath != "link" || res.Skipped[0].Reason != SkipSymlink {
		t.Fatalf("Skipped = %+v", res.Skipped)
	}
	a := res.Entries[0]
	fi, _ := os.Stat(filepath.Join(root, "a.txt"))
	if a.Kind != KindFile || a.Size != 5 || a.MtimeNS != fi.ModTime().UnixNano() || a.FileID.IsZero() {
		t.Fatalf("a.txt = %+v", a)
	}
	if d := res.Entries[2]; d.Kind != KindDir || d.Size != 0 || d.FileID.IsZero() {
		t.Fatalf("folder = %+v", d)
	}

	// A rename keeps the file id.
	if err := os.Rename(filepath.Join(root, "a.txt"), filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	st, err := Stat(root, "renamed.txt")
	if err != nil || st.FileID != a.FileID {
		t.Fatalf("after rename: %+v, %v; was %v", st, err, a.FileID)
	}
}

func TestScanSubtree(t *testing.T) {
	root := t.TempDir()
	write(t, root, "d/x/1.txt", "1")
	write(t, root, "d/x/2.txt", "2")
	write(t, root, "d/y.txt", "y")
	write(t, root, "other.txt", "o")
	write(t, root, "outside/secret.txt", "s")
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, "d", "link")); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"d/x":               {"d/x", "d/x/1.txt", "d/x/2.txt"},
		"d/y.txt":           {"d/y.txt"},
		"missing":           nil,
		"d/y.txt/below":     nil, // under a file
		"d/x/.DS_Store":     nil,
		"x.tmp/a":           nil,
		"d/link/secret.txt": nil, // reported skipped, below
	}
	for sub, want := range cases {
		res, err := Scan(root, sub)
		if err != nil {
			t.Fatalf("Scan(%q): %v", sub, err)
		}
		if !slices.Equal(paths(res), want) {
			t.Errorf("Scan(%q) = %q, want %q", sub, paths(res), want)
		}
	}
	res, _ := Scan(root, "d/link/secret.txt")
	if len(res.Skipped) != 1 || res.Skipped[0].RelPath != "d/link" || res.Skipped[0].Reason != SkipSymlink {
		t.Fatalf("through a symlink: %+v", res.Skipped)
	}
	for _, bad := range []string{"/abs", "a/../b", "a//b", "./a", "a/"} {
		if _, err := Scan(root, bad); err == nil {
			t.Errorf("Scan(%q) accepted", bad)
		}
	}
	if _, err := Scan(filepath.Join(root, "nope"), ""); err == nil {
		t.Error("Scan of a missing root succeeded")
	}
}

// A name created in NFD (as macOS apps may) is reported as on disk, and
// its key is the server's, the NFC one's.
func TestScanNFDName(t *testing.T) {
	root := t.TempDir()
	nfd := norm.NFD.String("Отчёт Café.txt")
	nfc := norm.NFC.String(nfd)
	if nfd == nfc {
		t.Fatal("test name has no decomposition")
	}
	write(t, root, nfd, "x")
	res, err := Scan(root, "")
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("Scan = %+v, %v", res, err)
	}
	got := res.Entries[0].RelPath
	if got != nfd {
		// Some file systems normalize on create (HFS+ to NFD, which this
		// already is); APFS, ext4 and NTFS keep the bytes.
		t.Logf("disk returned %q for NFD %q", got, nfd)
	}
	if NameKey(got) != strings.ToLower(nfc) {
		t.Fatalf("NameKey(%q) = %q, want %q", got, NameKey(got), strings.ToLower(nfc))
	}
	_, key, err := NormalizeName(got)
	if err != nil || key != NameKey(nfc) {
		t.Fatalf("NormalizeName(%q) = %q, %v", got, key, err)
	}
	// Partial scan by the on-disk name finds it.
	if res, err := Scan(root, got); err != nil || len(res.Entries) != 1 {
		t.Fatalf("Scan(%q) = %+v, %v", got, res, err)
	}
}

func TestHashCache(t *testing.T) {
	root := t.TempDir()
	write(t, root, "f.bin", "one")
	h, err := Hash(root, "f.bin", Hashed{})
	if err != nil || h.SHA256 != sum("one") || h.Size != 3 {
		t.Fatalf("Hash = %+v, %v", h, err)
	}
	// Unchanged stamp: the cached value comes back without reading (a
	// planted wrong hash proves it).
	cached := h
	cached.SHA256 = "cached"
	if got, err := Hash(root, "f.bin", cached); err != nil || got.SHA256 != "cached" {
		t.Fatalf("Hash with cache = %+v, %v", got, err)
	}
	// Same size, new mtime: rehashed.
	write(t, root, "f.bin", "two")
	later := time.Unix(0, h.MtimeNS).Add(time.Second)
	if err := os.Chtimes(filepath.Join(root, "f.bin"), later, later); err != nil {
		t.Fatal(err)
	}
	if got, err := Hash(root, "f.bin", cached); err != nil || got.SHA256 != sum("two") {
		t.Fatalf("Hash after edit = %+v, %v", got, err)
	}
	// Replaced by another file with the same size and mtime: a new file
	// id, rehashed.
	write(t, root, "g.bin", "333")
	now, _ := Hash(root, "f.bin", Hashed{})
	mt := time.Unix(0, now.MtimeNS)
	os.Chtimes(filepath.Join(root, "g.bin"), mt, mt)
	if err := os.Rename(filepath.Join(root, "g.bin"), filepath.Join(root, "f.bin")); err != nil {
		t.Fatal(err)
	}
	now.SHA256 = "stale"
	if got, err := Hash(root, "f.bin", now); err != nil || got.SHA256 != sum("333") {
		t.Fatalf("Hash after replace = %+v, %v", got, err)
	}
	if _, err := Hash(root, ".", Hashed{}); err == nil {
		t.Fatal("Hash of a folder succeeded")
	}
}

func TestWriteFileNewAndReplace(t *testing.T) {
	root := t.TempDir()
	mtime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	e, err := WriteFile(root, "doc.txt", "n1", strings.NewReader("v1"), CommitOptions{SHA256: sum("v1"), MTime: mtime})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "doc.txt")); string(got) != "v1" {
		t.Fatalf("content %q", got)
	}
	if e.MtimeNS != mtime.UnixNano() || e.Size != 2 {
		t.Fatalf("entry %+v", e)
	}
	// New file, but one appeared meanwhile.
	if _, err := WriteFile(root, "doc.txt", "n1", strings.NewReader("v1"), CommitOptions{}); !errors.Is(err, ErrChanged) {
		t.Fatalf("over an unexpected file: %v", err)
	}
	part, _ := PartPath(root, "doc.txt", "n1")
	os.Remove(part)

	// Replace what base says is there.
	base := e.Stamp
	e2, err := WriteFile(root, "doc.txt", "n1", strings.NewReader("v2"), CommitOptions{SHA256: sum("v2"), Expect: &base})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "doc.txt")); string(got) != "v2" {
		t.Fatalf("content %q", got)
	}
	if e2.FileID == base.FileID {
		t.Log("file id kept across replace (file system reuses inodes)")
	}

	// A local edit since base: not overwritten, the part is kept.
	write(t, root, "doc.txt", "local edit")
	stale := e2.Stamp
	_, err = WriteFile(root, "doc.txt", "n1", strings.NewReader("v3"), CommitOptions{Expect: &stale})
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("over a local edit: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "doc.txt")); string(got) != "local edit" {
		t.Fatalf("local edit lost: %q", got)
	}
	if got, err := os.ReadFile(part); err != nil || string(got) != "v3" {
		t.Fatalf("part after refusal: %q, %v", got, err)
	}

	// Deleted locally since base: refused too.
	os.Remove(filepath.Join(root, "doc.txt"))
	if _, err := Commit(root, "doc.txt", part, CommitOptions{Expect: &stale}); !errors.Is(err, ErrChanged) {
		t.Fatalf("over a local delete: %v", err)
	}
}

func TestCommitHashMismatchAndResume(t *testing.T) {
	root := t.TempDir()
	part, err := PartPath(root, "sub/big.bin", "n2")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(part) != ".~cloud-n2.part" || filepath.Dir(part) != filepath.Join(root, "sub") {
		t.Fatalf("PartPath = %s", part)
	}
	os.MkdirAll(filepath.Dir(part), 0o755)

	f, off, err := OpenPart(part)
	if err != nil || off != 0 {
		t.Fatalf("OpenPart = %d, %v", off, err)
	}
	f.WriteString("hello ")
	f.Close()
	// Restarted: resume from where it stopped.
	f, off, err = OpenPart(part)
	if err != nil || off != 6 {
		t.Fatalf("OpenPart after restart = %d, %v", off, err)
	}
	f.WriteString("world")
	f.Close()

	if _, err := Commit(root, "sub/big.bin", part, CommitOptions{SHA256: sum("something else")}); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("bad hash: %v", err)
	}
	if _, err := os.Stat(part); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("part kept after a hash mismatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub/big.bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("target written despite a hash mismatch")
	}
	if _, err := PartPath(root, "a", "x/y"); err == nil {
		t.Fatal("PartPath accepted an id with a separator")
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestRenameCaseOnly(t *testing.T) {
	root := t.TempDir()
	write(t, root, "report.txt", "x")
	before, _ := Stat(root, "report.txt")
	if err := Rename(root, "report.txt", "Report.TXT"); err != nil {
		t.Fatal(err)
	}
	if got := listDir(t, root); !slices.Equal(got, []string{"Report.TXT"}) {
		t.Fatalf("after case-only rename: %q", got)
	}
	after, err := Stat(root, "Report.TXT")
	if err != nil || after.FileID != before.FileID {
		t.Fatalf("file id %v → %v, %v", before.FileID, after.FileID, err)
	}

	// NFD → NFC only.
	nfd := norm.NFD.String("Café")
	write(t, root, nfd, "y")
	if err := Rename(root, nfd, norm.NFC.String(nfd)); err != nil {
		t.Fatal(err)
	}
	if got := listDir(t, root); !slices.Contains(got, norm.NFC.String(nfd)) {
		t.Fatalf("after NFD → NFC rename: %q", got)
	}

	// A crash between the two steps, reproduced by hand: the file is at
	// the temporary name; calling Rename again finishes.
	write(t, root, "b.txt", "z")
	h := sha256.Sum256([]byte("b.txt\x00B.txt"))
	tmp := tempPrefix + "rename-" + hex.EncodeToString(h[:8])
	if err := os.Rename(filepath.Join(root, "b.txt"), filepath.Join(root, tmp)); err != nil {
		t.Fatal(err)
	}
	if err := Rename(root, "b.txt", "B.txt"); err != nil {
		t.Fatalf("resuming: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "B.txt")); err != nil || string(got) != "z" {
		t.Fatalf("after resume: %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(root, tmp)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("temp name left behind")
	}
}

func TestRenameRefusesToReplace(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.txt", "a")
	write(t, root, "b.txt", "b")
	if err := Rename(root, "a.txt", "b.txt"); !errors.Is(err, ErrExists) {
		t.Fatalf("over another file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Rename(root, "a.txt", "dir/a.txt"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "dir", "a.txt")); string(got) != "a" {
		t.Fatalf("moved content %q", got)
	}
	if err := Rename(root, "../x", "y"); err == nil {
		t.Fatal("Rename outside the root accepted")
	}
}

func TestRemove(t *testing.T) {
	root := t.TempDir()
	write(t, root, "f.txt", "x")
	st, _ := Stat(root, "f.txt")

	write(t, root, "f.txt", "edited!")
	if err := Remove(root, "f.txt", st); !errors.Is(err, ErrChanged) {
		t.Fatalf("edited file: %v", err)
	}
	st, _ = Stat(root, "f.txt")
	if err := Remove(root, "f.txt", st); err != nil {
		t.Fatal(err)
	}
	if err := Remove(root, "f.txt", st); err != nil {
		t.Fatalf("already gone: %v", err)
	}

	write(t, root, "d/.DS_Store", "junk")
	write(t, root, "d/Thumbs.db", "junk")
	write(t, root, "d/keep.txt", "k")
	dst, _ := Stat(root, "d")
	if err := Remove(root, "d", dst); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("folder with a file: %v", err)
	}
	// A lock file means a document is open: not junk.
	os.Remove(filepath.Join(root, "d", "keep.txt"))
	write(t, root, "d/~$doc.docx", "lock")
	if err := Remove(root, "d", dst); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("folder with a lock file: %v", err)
	}
	os.Remove(filepath.Join(root, "d", "~$doc.docx"))
	if err := Remove(root, "d", dst); err != nil {
		t.Fatalf("folder with junk only: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "d")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("folder still there")
	}
	// A folder replaced by a file of the same name is not the expected one.
	write(t, root, "d", "now a file")
	if err := Remove(root, "d", dst); !errors.Is(err, ErrChanged) {
		t.Fatalf("folder replaced by a file: %v", err)
	}
}

func TestMarker(t *testing.T) {
	root := t.TempDir()
	if err := CheckMarker(root, "c1"); !errors.Is(err, ErrNoMarker) {
		t.Fatalf("no marker: %v", err)
	}
	if err := CheckMarker(filepath.Join(root, "unplugged"), "c1"); !errors.Is(err, ErrNoMarker) {
		t.Fatalf("missing folder: %v", err)
	}
	if err := CreateMarker(root, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := CreateMarker(root, "c1"); err != nil {
		t.Fatalf("again: %v", err)
	}
	if err := CheckMarker(root, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := CheckMarker(root, "c2"); !errors.Is(err, ErrMarkerMismatch) {
		t.Fatalf("other connection: %v", err)
	}
	if err := CreateMarker(root, "c2"); !errors.Is(err, ErrMarkerMismatch) {
		t.Fatalf("create over another connection's: %v", err)
	}
	// The marker itself is never synced.
	if res, _ := Scan(root, ""); len(res.Entries) != 0 {
		t.Fatalf("Scan sees %q", paths(res))
	}
}
