package fs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/do-service/cloud/backend/internal/access"
)

// An upload by node id replaces that file wherever it went meanwhile, and
// only while it has the ETag the client based its change on.
func TestUploadByNodeID(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "Docs")
	f := e.file(t, docs.ID, "a.txt", "v1")
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)

	up, err := e.svc.ReserveUpload(ctx, e.user, Upload{NodeID: f.ID, IfMatch: f.ETag, Size: 5, MTime: mtime, Name: "ignored", ParentID: e.root.ID})
	if err != nil || up.ParentID != docs.ID || up.Name != "a.txt" || e.reserved(t) != 5 {
		t.Fatalf("reserve: %+v, %v, reserved %d", up, err, e.reserved(t))
	}
	// Renamed and moved while the upload runs: same file, same ETag.
	if _, err := e.svc.Move(ctx, e.user, f.ID, e.root.ID, "b.txt"); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.FinishUpload(ctx, e.user, up.ID, e.stage(t, up.ID, "v2new"))
	if err != nil || n.ID != f.ID || n.Name != "b.txt" || n.ParentID != e.root.ID || n.Size != 5 || n.ETag == f.ETag || !n.MTime.Equal(mtime) {
		t.Fatalf("finish: %+v, %v", n, err)
	}
	if got := e.get(t, f.ID); got.ETag != n.ETag || !got.MTime.Equal(mtime) {
		t.Fatalf("stored: %+v", got)
	}
	if got, ok := e.onDisk(t, "b.txt"); !ok || got != "v2new" {
		t.Fatalf("on disk: %q, %v", got, ok)
	}
	if versions, _, err := e.svc.ListVersions(ctx, e.user, f.ID); err != nil || len(versions) != 1 || versions[0].Size != 2 {
		t.Fatalf("versions: %+v, %v", versions, err)
	}
	if e.reserved(t) != 0 || e.usedBytes(t) != 7 {
		t.Fatalf("reserved %d, used %d", e.reserved(t), e.usedBytes(t))
	}
	e.checkSizes(t)
}

func TestUploadByNodeIDConflicts(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	f := e.file(t, e.root.ID, "a.txt", "base")
	reserve := func(etag string) Upload {
		t.Helper()
		up, err := e.svc.ReserveUpload(ctx, e.user, Upload{NodeID: f.ID, IfMatch: etag, Size: 2})
		if err != nil {
			t.Fatal(err)
		}
		return up
	}

	// Two clients edit the same base: the first wins, the second learns
	// at finish that its base is gone.
	first, second := reserve(f.ETag), reserve(f.ETag)
	won, err := e.svc.FinishUpload(ctx, e.user, first.ID, e.stage(t, first.ID, "c1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.FinishUpload(ctx, e.user, second.ID, e.stage(t, second.ID, "c2")); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("second edit: %v", err)
	}
	if got := e.read(t, f.ID, nil); got != "c1" || e.get(t, f.ID).ETag != won.ETag {
		t.Fatalf("content %q", got)
	}
	// The failed finish keeps the reservation for the caller to cancel,
	// as any failed finish does; its content is gone.
	if _, err := e.svc.GetUpload(ctx, e.user, second.ID); err != nil {
		t.Fatalf("reservation: %v", err)
	}
	if err := e.svc.CancelUpload(ctx, e.user, second.ID); err != nil || e.reserved(t) != 0 {
		t.Fatalf("cancel: %v, reserved %d", err, e.reserved(t))
	}
	if _, err := e.svc.store.Stat(ctx, "uploads/tus/"+second.ID); err == nil {
		t.Fatal("staged content left")
	}

	// A stale base is refused upfront, before any byte is sent.
	if _, err := e.svc.ReserveUpload(ctx, e.user, Upload{NodeID: f.ID, IfMatch: f.ETag, Size: 2}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale at reserve: %v", err)
	}
	// Without ifMatch it just replaces.
	plain := reserve("")
	if _, err := e.svc.FinishUpload(ctx, e.user, plain.ID, e.stage(t, plain.ID, "c3")); err != nil {
		t.Fatal(err)
	}

	bob := e.newUser(t, "bob")
	shared := e.mkdir(t, e.root.ID, "Shared")
	inShared := e.file(t, shared.ID, "s.txt", "s")
	e.share(t, shared.ID, bob, access.Read)
	link := e.uploadLink(t, shared.ID)
	before := reserve("")
	e.trash(t, f.ID)
	for _, tc := range []struct {
		name  string
		actor string
		up    Upload
		want  error
	}{
		{"folder", e.user, Upload{NodeID: shared.ID, Size: 1}, ErrNotFile},
		{"read-only recipient", bob, Upload{NodeID: inShared.ID, Size: 1}, ErrForbidden},
		{"upload link", link, Upload{NodeID: inShared.ID, Size: 1}, ErrForbidden},
		{"trashed", e.user, Upload{NodeID: f.ID, Size: 1}, ErrNotFound},
		{"bad id", e.user, Upload{NodeID: "x", Size: 1}, ErrNotFound},
	} {
		if _, err := e.svc.ReserveUpload(ctx, tc.actor, tc.up); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: e.root.ID, Name: "x", IfMatch: "x", OnConflict: ConflictFail}); err == nil {
		t.Error("IfMatch without NodeID accepted")
	}
	// Trashed while uploading: there is nothing to replace.
	if _, err := e.svc.FinishUpload(ctx, e.user, before.ID, e.stage(t, before.ID, "xx")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finish on trashed: %v", err)
	}
	e.checkSizes(t)
}

// mtime is kept for a new file as well.
func TestUploadMTime(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	mtime := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	up, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: e.root.ID, Name: "old.txt", Size: 1, OnConflict: ConflictFail, MTime: mtime})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.GetUpload(ctx, e.user, up.ID); err != nil || !got.MTime.Equal(mtime) {
		t.Fatalf("kept: %+v, %v", got, err)
	}
	n, err := e.svc.FinishUpload(ctx, e.user, up.ID, e.stage(t, up.ID, "x"))
	if err != nil || !n.MTime.Equal(mtime) || !e.get(t, n.ID).MTime.Equal(mtime) {
		t.Fatalf("finish: %+v, %v", n, err)
	}
	// The folder's mtime is the server's: it changed now.
	if root := e.get(t, e.root.ID); time.Since(root.MTime) > time.Minute {
		t.Fatalf("root mtime %v", root.MTime)
	}
}

func TestMoveAndTrashIfMatch(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "Docs")
	f := e.file(t, docs.ID, "a.txt", "a")
	docs = e.get(t, docs.ID)

	if _, err := e.svc.MoveIfMatch(ctx, e.user, f.ID, "", "b.txt", []string{"other"}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale rename: %v", err)
	}
	n, err := e.svc.MoveIfMatch(ctx, e.user, f.ID, "", "b.txt", []string{"other", f.ETag})
	if err != nil || n.Name != "b.txt" {
		t.Fatalf("rename: %+v, %v", n, err)
	}
	// Renaming a file leaves its ETag: it follows the content.
	if n.ETag != f.ETag {
		t.Fatalf("etag changed on rename")
	}
	if _, err := e.svc.MoveIfMatch(ctx, e.user, f.ID, e.root.ID, "", []string{"*"}); err != nil {
		t.Fatalf("move with *: %v", err)
	}
	// The folder's ETag moved on with what left it.
	if err := e.svc.TrashIfMatch(ctx, e.user, docs.ID, []string{docs.ETag}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale trash: %v", err)
	}
	if got := e.trashNames(t); len(got) != 0 {
		t.Fatalf("trashed anyway: %v", got)
	}
	if err := e.svc.TrashIfMatch(ctx, e.user, docs.ID, []string{e.get(t, docs.ID).ETag}); err != nil {
		t.Fatalf("trash: %v", err)
	}
	if err := e.svc.TrashIfMatch(ctx, e.user, f.ID, nil); err != nil {
		t.Fatalf("trash without condition: %v", err)
	}
	if got := strings.Join(e.trashNames(t), ","); !strings.Contains(got, "Docs") || !strings.Contains(got, "b.txt") {
		t.Fatalf("trash: %s", got)
	}
}
