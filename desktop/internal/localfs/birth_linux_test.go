package localfs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A file deleted and another one created get the same inode on ext4 (and
// tmpfs, overlayfs on them): the birth time tells them apart. A rename
// keeps it.
func TestFileIDInodeReuse(t *testing.T) {
	root := t.TempDir()
	first, err := writeStat(root, "first")
	if err != nil {
		t.Fatal(err)
	}
	if first.Birth == 0 {
		t.Skip("the file system doesn't report birth times")
	}
	for i := range 1000 {
		old, err := writeStat(root, "old")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "old")); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("new%d", i)
		cur, err := writeStat(root, name)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Index != old.Index {
			continue
		}
		if cur == old || cur.Matches(old) {
			t.Fatalf("a new file with a reused inode has the deleted one's id: %+v", cur)
		}
		if err := os.Rename(filepath.Join(root, name), filepath.Join(root, "moved")); err != nil {
			t.Fatal(err)
		}
		if moved, err := Stat(root, "moved"); err != nil || moved.FileID != cur {
			t.Fatalf("after a rename: %+v, %v; was %+v", moved.FileID, err, cur)
		}
		return
	}
	t.Skip("no inode was reused")
}

func writeStat(root, name string) (FileID, error) {
	if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
		return FileID{}, err
	}
	st, err := Stat(root, name)
	return st.FileID, err
}
