package fs

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/storage"
)

// upload runs content through the upload path into parentID as name.
func (e env) upload(t *testing.T, parentID, name, content string, policy Conflict) Node {
	t.Helper()
	ctx := context.Background()
	up, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: parentID, Name: name, Size: int64(len(content)), OnConflict: policy})
	if err != nil {
		t.Fatalf("reserve %s: %v", name, err)
	}
	n, err := e.svc.FinishUpload(ctx, e.user, up.ID, e.stage(t, up.ID, content))
	if err != nil {
		t.Fatalf("finish %s: %v", name, err)
	}
	return n
}

func (e env) readVersion(t *testing.T, id string) (string, Node) {
	t.Helper()
	rc, n, err := e.svc.OpenVersion(context.Background(), e.user, id, func(Node) (*storage.Range, error) { return nil, nil })
	if err != nil {
		t.Fatalf("open version: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	return string(data), n
}

func TestVersions(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "Docs")
	first := e.file(t, docs.ID, "a.txt", "v1")

	second := e.upload(t, docs.ID, "A.TXT", "version 2", ConflictReplace)
	if second.ID != first.ID || second.Size != 9 || second.ETag == first.ETag || second.Name != "a.txt" {
		t.Fatalf("replaced: %+v", second)
	}
	if got, _ := e.onDisk(t, "Docs/a.txt"); got != "version 2" {
		t.Fatalf("current on disk: %q", got)
	}
	versions, _, err := e.svc.ListVersions(ctx, e.user, first.ID)
	sum := sha256.Sum256([]byte("v1"))
	if err != nil || len(versions) != 1 || versions[0].Size != 2 || string(versions[0].Hash) != string(sum[:]) {
		t.Fatalf("versions: %+v, %v", versions, err)
	}
	if !versions[0].CreatedAt.Equal(first.MTime) {
		t.Errorf("version time %s, want the old mtime %s", versions[0].CreatedAt, first.MTime)
	}
	if data, err := os.ReadFile(filepath.Join(e.dataDir, "versions", first.ID, versions[0].ID)); err != nil || string(data) != "v1" {
		t.Fatalf("version on disk: %q, %v", data, err)
	}
	// Versions count in the quota.
	if used := e.usedBytes(t); used != 2+9 {
		t.Fatalf("used %d", used)
	}
	e.checkSizes(t)
	if d := e.get(t, docs.ID); d.Size != 9 {
		t.Fatalf("folder size %d", d.Size)
	}

	e.upload(t, docs.ID, "a.txt", "v3", ConflictReplace)
	versions, _, _ = e.svc.ListVersions(ctx, e.user, first.ID)
	if len(versions) != 2 {
		t.Fatalf("after another replace: %d versions", len(versions))
	}
	oldest := versions[1]
	if got, n := e.readVersion(t, oldest.ID); got != "v1" || n.ETag != oldest.ID || n.Size != 2 {
		t.Fatalf("read oldest: %q, %+v", got, n)
	}

	// Versions follow the file wherever it goes.
	if _, err := e.svc.Move(ctx, e.user, first.ID, e.root.ID, "moved.txt"); err != nil {
		t.Fatal(err)
	}
	restored, err := e.svc.RestoreVersion(ctx, e.user, oldest.ID)
	if err != nil || restored.Size != 2 {
		t.Fatalf("restore: %+v, %v", restored, err)
	}
	if got := e.read(t, first.ID, nil); got != "v1" {
		t.Fatalf("restored content %q", got)
	}
	// Restoring adds to the history: v1, version 2, v3 are all kept.
	if versions, _, _ = e.svc.ListVersions(ctx, e.user, first.ID); len(versions) != 3 {
		t.Fatalf("after restore: %d versions", len(versions))
	}
	if got, _ := e.readVersion(t, oldest.ID); got != "v1" {
		t.Fatalf("restored version changed: %q", got)
	}
	e.checkSizes(t)

	e.trash(t, first.ID)
	if err := e.svc.Purge(ctx, e.user, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "versions", first.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("versions left on disk: %v", err)
	}
	var blobs, rows int
	if err := e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM blobs), (SELECT count(*) FROM versions)`).Scan(&blobs, &rows); err != nil || blobs != 0 || rows != 0 {
		t.Fatalf("after purge: %d blobs, %d versions, %v", blobs, rows, err)
	}
	if used := e.usedBytes(t); used != 0 {
		t.Fatalf("used after purge %d", used)
	}
}

// Each version is credited to whoever wrote the content it keeps.
func TestVersionAuthors(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE users SET display_name = 'Алиса' WHERE id = $1`, e.user); err != nil {
		t.Fatal(err)
	}
	shared := e.mkdir(t, e.root.ID, "Shared")
	f := e.file(t, shared.ID, "doc.txt", "alice wrote")
	bob := e.newUser(t, "bob")
	e.share(t, shared.ID, bob, access.Read|access.Write)
	alice, bobAuthor := Author{ID: e.user, Name: "Алиса"}, Author{ID: bob, Name: "bob"}

	if _, current, err := e.svc.ListVersions(ctx, e.user, f.ID); err != nil || current != alice {
		t.Fatalf("new file: %+v, %v", current, err)
	}
	if _, err := e.svc.WriteFile(ctx, bob, f.ID, strings.NewReader("bob wrote"), 9, PutCond{}); err != nil {
		t.Fatal(err)
	}
	versions, current, err := e.svc.ListVersions(ctx, e.user, f.ID)
	if err != nil || current != bobAuthor || len(versions) != 1 || versions[0].Author != alice {
		t.Fatalf("after bob's edit: %+v, %+v, %v", current, versions, err)
	}
	// Restoring writes the old content anew: it's the restorer's.
	if _, err := e.svc.RestoreVersion(ctx, e.user, versions[0].ID); err != nil {
		t.Fatal(err)
	}
	versions, current, _ = e.svc.ListVersions(ctx, e.user, f.ID)
	if current != alice || len(versions) != 2 || versions[0].Author != bobAuthor || versions[1].Author != alice {
		t.Fatalf("after restore: %+v, %+v", current, versions)
	}

	// Content from before authors were kept, or by a deleted account, has
	// none.
	if _, err := e.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, bob); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE nodes SET modified_by = NULL WHERE id = $1`, f.ID); err != nil {
		t.Fatal(err)
	}
	versions, current, _ = e.svc.ListVersions(ctx, e.user, f.ID)
	if current != (Author{}) || versions[0].Author != (Author{}) || versions[1].Author != alice {
		t.Fatalf("unknown authors: %+v, %+v", current, versions)
	}
}

func TestReplaceRules(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.mkdir(t, e.root.ID, "dir")
	if _, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: e.root.ID, Name: "DIR", Size: 1, OnConflict: ConflictReplace}); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("replace a folder: %v", err)
	}
	// Nothing to replace: a new file.
	if n := e.upload(t, e.root.ID, "new.txt", "x", ConflictReplace); n.Name != "new.txt" {
		t.Fatalf("new file: %+v", n)
	}
	if _, _, err := e.svc.ListVersions(ctx, e.user, e.root.ID); !errors.Is(err, ErrNotFile) {
		t.Fatalf("versions of a folder: %v", err)
	}
	f := e.file(t, e.root.ID, "f.txt", "1")
	e.upload(t, e.root.ID, "f.txt", "2", ConflictReplace)
	versions, _, _ := e.svc.ListVersions(ctx, e.user, f.ID)
	bob := e.newUser(t, "bob")
	for what, err := range map[string]error{
		"bob lists": func() error { _, _, err := e.svc.ListVersions(ctx, bob, f.ID); return err }(),
		"bob reads": func() error {
			_, _, err := e.svc.OpenVersion(ctx, bob, versions[0].ID, func(Node) (*storage.Range, error) { return nil, nil })
			return err
		}(),
		"bob restores": func() error { _, err := e.svc.RestoreVersion(ctx, bob, versions[0].ID); return err }(),
		"bad id":       func() error { _, err := e.svc.RestoreVersion(ctx, e.user, "x"); return err }(),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", what, err)
		}
	}
	// A trashed file's versions are out of reach until it's restored.
	e.trash(t, f.ID)
	if _, err := e.svc.RestoreVersion(ctx, e.user, versions[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("restore a version of a trashed file: %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(e.dataDir, "uploads")); len(left) != 1 {
		// Only tus/ from e.stage: a failed restore leaves no copy behind.
		t.Errorf("uploads/ holds %d entries", len(left))
	}
}

func TestVersionsFlat(t *testing.T) {
	e := newEnv(t, func(b storage.Backend) storage.Backend { return flatStore{b} })
	ctx := context.Background()
	f := e.file(t, e.root.ID, "a.txt", "one")
	e.upload(t, e.root.ID, "a.txt", "two", ConflictReplace)
	versions, _, err := e.svc.ListVersions(ctx, e.user, f.ID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("versions: %v, %v", versions, err)
	}
	if got, _ := e.readVersion(t, versions[0].ID); got != "one" {
		t.Fatalf("version %q", got)
	}
	if _, err := e.svc.RestoreVersion(ctx, e.user, versions[0].ID); err != nil || e.read(t, f.ID, nil) != "one" {
		t.Fatalf("restore: %v", err)
	}
}
