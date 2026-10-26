package fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/do-service/cloud/backend/internal/storage"
)

func (e env) usedBytes(t *testing.T) int64 {
	t.Helper()
	var used int64
	if err := e.pool.QueryRow(context.Background(), `SELECT used_bytes FROM users WHERE id = $1`, e.user).Scan(&used); err != nil {
		t.Fatal(err)
	}
	return used
}

func (e env) trash(t *testing.T, id string) {
	t.Helper()
	if err := e.svc.Trash(context.Background(), e.user, id); err != nil {
		t.Fatalf("trash %s: %v", id, err)
	}
}

func (e env) trashNames(t *testing.T) []string {
	t.Helper()
	items, _, err := e.svc.ListTrash(context.Background(), e.user, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range items {
		names = append(names, it.OriginalPath+"|"+it.Name)
	}
	return names
}

func (e env) inTrashArea(t *testing.T, id string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(e.dataDir, "trash", id))
	return err == nil
}

func TestTrashAndRestore(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "Docs")
	sub := e.mkdir(t, docs.ID, "Sub")
	a := e.file(t, sub.ID, "a.txt", "aaa")
	e.file(t, docs.ID, "b.txt", "bb")

	e.trash(t, docs.ID)
	e.checkSizes(t)
	if _, err := e.svc.Get(ctx, e.user, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("trashed child still visible: %v", err)
	}
	if _, ok := e.onDisk(t, "Docs/b.txt"); ok || !e.inTrashArea(t, docs.ID) {
		t.Fatal("content not moved to trash/")
	}
	if got := e.get(t, e.root.ID).Size; got != 0 {
		t.Fatalf("root size after trash: %d", got)
	}
	if used := e.usedBytes(t); used != 5 {
		t.Fatalf("trash still counts in quota: used %d, want 5", used)
	}
	// The name is free again, in the database and on disk.
	again := e.mkdir(t, e.root.ID, "docs")
	e.file(t, again.ID, "new.txt", "n")

	restored, err := e.svc.Restore(ctx, e.user, docs.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Name != "Docs (2)" || restored.ParentID != e.root.ID {
		t.Fatalf("restored as %+v", restored)
	}
	if got, ok := e.onDisk(t, "Docs (2)/Sub/a.txt"); !ok || got != "aaa" {
		t.Fatalf("content after restore: %q, %v", got, ok)
	}
	if n := e.get(t, a.ID); n.ParentID != sub.ID {
		t.Fatalf("descendant not restored: %+v", n)
	}
	e.checkSizes(t)
	if len(e.trashNames(t)) != 0 {
		t.Fatalf("trash after restore: %v", e.trashNames(t))
	}
}

// A node trashed on its own stays a separate item when its folder is
// trashed and restored later; restored itself into a trashed folder, it
// lands in the root.
func TestTrashNested(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "Docs")
	sub := e.mkdir(t, docs.ID, "Sub")
	old := e.file(t, sub.ID, "old.txt", "o")
	keep := e.file(t, sub.ID, "keep.txt", "k")

	e.trash(t, old.ID)
	e.trash(t, docs.ID)
	if got, want := e.trashNames(t), []string{"|Docs", "Docs/Sub|old.txt"}; !slices.Equal(got, want) {
		t.Fatalf("trash %v, want %v", got, want)
	}
	page, next, err := e.svc.ListTrash(ctx, e.user, 1, "")
	if err != nil || len(page) != 1 || next == "" {
		t.Fatalf("first page: %v %q %v", page, next, err)
	}
	if page, next, err = e.svc.ListTrash(ctx, e.user, 1, next); err != nil || len(page) != 1 || page[0].ID != old.ID || next != "" {
		t.Fatalf("second page: %v %q %v", page, next, err)
	}

	// Docs comes back without old.txt, which was deleted on its own.
	if _, err := e.svc.Restore(ctx, e.user, docs.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Get(ctx, e.user, old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("separately trashed file came back with its folder: %v", err)
	}
	if n := e.get(t, keep.ID); n.ParentID != sub.ID {
		t.Fatalf("keep.txt: %+v", n)
	}
	// With its folder in the trash again, old.txt is restored to the root.
	e.trash(t, docs.ID)
	if _, err := e.svc.Restore(ctx, e.user, old.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.get(t, old.ID); n.ParentID != e.root.ID {
		t.Fatalf("restored into a trashed folder: %+v", n)
	}
	if got, ok := e.onDisk(t, "old.txt"); !ok || got != "o" {
		t.Fatal("old.txt not back on disk")
	}
	e.checkSizes(t)
}

func TestPurge(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "Docs")
	inner := e.file(t, docs.ID, "inner.txt", "1234")
	e.file(t, docs.ID, "other.txt", "56")
	top := e.file(t, e.root.ID, "top.txt", "789")

	e.trash(t, inner.ID)
	e.trash(t, docs.ID)
	e.trash(t, top.ID)
	if err := e.svc.Purge(ctx, e.user, docs.ID); err != nil {
		t.Fatal(err)
	}
	if e.inTrashArea(t, docs.ID) || e.inTrashArea(t, inner.ID) {
		t.Fatal("purged content left in trash/")
	}
	if got := e.trashNames(t); !slices.Equal(got, []string{"|top.txt"}) {
		t.Fatalf("trash after purge: %v", got)
	}
	if used := e.usedBytes(t); used != 3 {
		t.Fatalf("used_bytes %d, want 3", used)
	}
	var blobs int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM blobs`).Scan(&blobs); err != nil || blobs != 1 {
		t.Fatalf("blobs left: %d, %v", blobs, err)
	}

	if err := e.svc.EmptyTrash(ctx, e.user); err != nil {
		t.Fatal(err)
	}
	if used := e.usedBytes(t); used != 0 || len(e.trashNames(t)) != 0 || e.inTrashArea(t, top.ID) {
		t.Fatalf("after emptying: used %d, trash %v", used, e.trashNames(t))
	}
}

func TestTrashFlat(t *testing.T) {
	e := newEnv(t, func(b storage.Backend) storage.Backend { return flatStore{b} })
	ctx := context.Background()
	f := e.file(t, e.root.ID, "a.txt", "flat")
	e.trash(t, f.ID)
	if _, err := e.svc.Restore(ctx, e.user, f.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.read(t, f.ID, nil); got != "flat" {
		t.Fatalf("after restore: %q", got)
	}
	e.trash(t, f.ID)
	if err := e.svc.Purge(ctx, e.user, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, flatKey(f.blobID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("flat object left after purge: %v", err)
	}
}

func TestTrashRejects(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	f := e.file(t, e.root.ID, "a.txt", "a")
	bob := e.newUser(t, "bob")
	if err := e.svc.Trash(ctx, e.user, e.root.ID); !errors.Is(err, ErrIsRoot) {
		t.Errorf("trash root: %v", err)
	}
	for what, err := range map[string]error{
		"bob trashes":          e.svc.Trash(ctx, bob, f.ID),
		"restore a live node":  func() error { _, err := e.svc.Restore(ctx, e.user, f.ID); return err }(),
		"purge a live node":    e.svc.Purge(ctx, e.user, f.ID),
		"trash a malformed id": e.svc.Trash(ctx, e.user, "x"),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", what, err)
		}
	}
	e.trash(t, f.ID)
	for what, err := range map[string]error{
		"bob restores": func() error { _, err := e.svc.Restore(ctx, bob, f.ID); return err }(),
		"bob purges":   e.svc.Purge(ctx, bob, f.ID),
		"trash again":  e.svc.Trash(ctx, e.user, f.ID),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", what, err)
		}
	}
	if _, _, err := e.svc.ListTrash(ctx, e.user, 0, "garbage"); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("bad cursor: %v", err)
	}
	if items, _, err := e.svc.ListTrash(ctx, bob, 0, ""); err != nil || len(items) != 0 {
		t.Errorf("bob sees %v, %v", items, err)
	}
}

func TestFreeName(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	for _, name := range []string{"a.txt", "a (2).txt", "Docs", ".bashrc"} {
		e.file(t, e.root.ID, name, "")
	}
	for in, want := range map[string]string{"a.txt": "a (3).txt", "Docs": "Docs (2)", ".bashrc": ".bashrc (2)", "free.txt": "free.txt"} {
		if got, _, err := freeName(ctx, e.pool, e.root.ID, in); err != nil || got != want {
			t.Errorf("freeName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
