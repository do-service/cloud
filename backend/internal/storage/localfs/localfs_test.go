package localfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/do-service/cloud/backend/internal/storage"
	"github.com/do-service/cloud/backend/internal/storage/storagetest"
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestContract(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Backend { return open(t, t.TempDir()) })
}

func TestOpenCreatesLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	open(t, dir)
	for _, a := range Areas {
		if fi, err := os.Stat(filepath.Join(dir, a)); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", a, err)
		}
	}
	// Reopening an existing store is the normal restart path.
	open(t, dir)
}

// The files are the product (Q4): an admin must find them under the user's
// own names, not under ids.
func TestFilesAreReadableOnDisk(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := s.Put(context.Background(), "files/u1/Документы/Отчёт.txt", strings.NewReader("привет"), -1); err != nil {
		t.Fatalf("put: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "files", "u1", "Документы", "Отчёт.txt"))
	if err != nil || string(data) != "привет" {
		t.Fatalf("on disk: %q, %v", data, err)
	}
}

func TestFailedPutLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := s.Put(context.Background(), "files/u1/a", strings.NewReader("abc"), 10); !errors.Is(err, storage.ErrSizeMismatch) {
		t.Fatalf("put: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, tmpDir))
	if err != nil {
		t.Fatalf("read uploads: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

// A symlink planted on disk (by an admin, a restored backup, a bug) must
// not let any key reach outside the data dir.
func TestSymlinkCannotEscape(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir)
	if err := os.Symlink(outside, filepath.Join(dir, "files", "evil")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Get(ctx, "files/evil/secret", nil); err == nil {
		t.Fatal("read through a symlink out of the store")
	}
	if _, err := s.Put(ctx, "files/evil/planted", strings.NewReader("x"), -1); err == nil {
		t.Fatal("wrote through a symlink out of the store")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("file appeared outside: %v", err)
	}
}

func TestCheckSameFS(t *testing.T) {
	devs := map[string]uint64{".": 1, "files": 1, "versions": 1, "trash": 2}
	device := func(name string) (uint64, error) { return devs[name], nil }

	if err := checkSameFS(device, []string{".", "files", "versions"}); err != nil {
		t.Fatalf("same device: %v", err)
	}
	err := checkSameFS(device, []string{".", "files", "trash"})
	if err == nil || !strings.Contains(err.Error(), "trash") {
		t.Fatalf("separate trash mount: err %v", err)
	}
}
