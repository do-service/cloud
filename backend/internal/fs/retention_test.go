package fs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestExpiredTrash(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	old, fresh := e.file(t, e.root.ID, "old.txt", "o"), e.file(t, e.root.ID, "fresh.txt", "f")
	docs := e.mkdir(t, e.root.ID, "Docs")
	inner := e.file(t, docs.ID, "inner.txt", "i")
	e.trash(t, old.ID)
	e.trash(t, fresh.ID)
	e.trash(t, docs.ID)
	if _, err := e.pool.Exec(ctx, `UPDATE nodes SET trashed_at = now() - interval '40 days' WHERE id = ANY($1::uuid[])`, []string{old.ID, docs.ID, inner.ID}); err != nil {
		t.Fatal(err)
	}
	refs, err := e.svc.ExpiredTrash(ctx, time.Now().Add(-30*24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range refs {
		if r.OwnerID != e.user {
			t.Errorf("owner %s", r.OwnerID)
		}
		ids = append(ids, r.ID)
	}
	// Trash items only, not what was deleted along with them.
	slices.Sort(ids)
	want := []string{old.ID, docs.ID}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("expired %v, want %v", ids, want)
	}
}

func TestPruneVersions(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	f := e.file(t, e.root.ID, "a.txt", "v1")
	for _, c := range []string{"v2", "v3", "v4"} {
		e.upload(t, e.root.ID, "a.txt", c, ConflictReplace)
	}
	g := e.file(t, e.root.ID, "b.txt", "b1")
	e.upload(t, e.root.ID, "b.txt", "b2", ConflictReplace)
	usedBefore := e.usedBytes(t)

	if n, err := e.svc.PruneVersions(ctx, VersionPolicy{}, 100); err != nil || n != 0 {
		t.Fatalf("no policy pruned %d, %v", n, err)
	}
	// Keep two per file: a.txt drops one (v1, v2, v3 → v2, v3), b.txt keeps
	// its only one.
	n, err := e.svc.PruneVersions(ctx, VersionPolicy{MaxCount: 2}, 100)
	if err != nil || n != 1 {
		t.Fatalf("by count: %d, %v", n, err)
	}
	versions, _, _ := e.svc.ListVersions(ctx, e.user, f.ID)
	var contents []string
	for _, v := range versions {
		got, _ := e.readVersion(t, v.ID)
		contents = append(contents, got)
	}
	if !slices.Equal(contents, []string{"v3", "v2"}) {
		t.Fatalf("kept %v", contents)
	}
	if used := e.usedBytes(t); used != usedBefore-2 {
		t.Fatalf("used %d, want %d", used, usedBefore-2)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dataDir, "versions", f.ID)); len(entries) != 2 {
		t.Fatalf("%d version files on disk", len(entries))
	}

	// By age: everything replaced over a day ago goes; b.txt's version was
	// replaced just now and stays, however old its content's mtime (a sync
	// client sets that).
	if _, err := e.pool.Exec(ctx, `UPDATE versions SET replaced_at = now() - interval '3 days' WHERE node_id = $1`, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE versions SET created_at = now() - interval '3 years' WHERE node_id = $1`, g.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.PruneVersions(ctx, VersionPolicy{MaxAge: 24 * time.Hour}, 1); err != nil || n != 1 {
		t.Fatalf("limit 1: %d, %v", n, err)
	}
	if n, err := e.svc.PruneVersions(ctx, VersionPolicy{MaxAge: 24 * time.Hour}, 100); err != nil || n != 1 {
		t.Fatalf("by age: %d, %v", n, err)
	}
	if v, _, _ := e.svc.ListVersions(ctx, e.user, f.ID); len(v) != 0 {
		t.Fatalf("a.txt keeps %d versions", len(v))
	}
	if v, _, _ := e.svc.ListVersions(ctx, e.user, g.ID); len(v) != 1 {
		t.Fatalf("b.txt keeps %d versions", len(v))
	}
	if got := e.read(t, f.ID, nil); got != "v4" {
		t.Fatalf("current content touched: %q", got)
	}
	e.checkSizes(t)
}
