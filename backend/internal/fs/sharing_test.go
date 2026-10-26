package fs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/do-service/cloud/backend/internal/access"
)

func (e env) share(t *testing.T, nodeID, userID string, p access.Perm) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO shares (node_id, grantee_type, grantee_id, perms) VALUES ($1, 'user', $2, $3)`, nodeID, userID, int16(p)); err != nil {
		t.Fatal(err)
	}
}

// What a recipient may do with each kind of share, in the owner's tree.
func TestRecipient(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	projects := e.mkdir(t, e.root.ID, "Projects")
	shared := e.mkdir(t, projects.ID, "Shared")
	doc := e.file(t, shared.ID, "doc.txt", "hello")
	bob := e.newUser(t, "bob")
	bobRoot, err := e.svc.EnsureRoot(ctx, bob)
	if err != nil {
		t.Fatal(err)
	}

	e.share(t, shared.ID, bob, access.Read)
	if n, err := e.svc.Get(ctx, bob, doc.ID); err != nil || n.Perms != access.Read {
		t.Fatalf("read-only get: %+v, %v", n.Perms, err)
	}
	items, _, err := e.svc.List(ctx, bob, shared.ID, ListOptions{})
	if err != nil || len(items) != 1 || items[0].Perms != access.Read {
		t.Fatalf("read-only list: %v, %v", items, err)
	}
	if got := e.read(t, doc.ID, nil); got != "hello" {
		t.Fatalf("owner read %q", got)
	}
	if rc, _, err := e.svc.Open(ctx, bob, doc.ID, nil); err != nil {
		t.Fatalf("read-only open: %v", err)
	} else {
		rc.Close()
	}
	// The breadcrumbs start at the shared folder.
	path, err := e.svc.Path(ctx, bob, doc.ID)
	if err != nil || len(path) != 2 || path[0].ID != shared.ID {
		t.Fatalf("recipient path: %+v, %v", path, err)
	}
	if _, err := e.svc.Get(ctx, bob, projects.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("above the share: %v", err)
	}
	for what, err := range map[string]error{
		"mkdir": func() error { _, err := e.svc.CreateDir(ctx, bob, shared.ID, "x"); return err }(),
		"upload": func() error {
			_, err := e.svc.CreateFile(ctx, bob, shared.ID, "x", strings.NewReader(""), 0)
			return err
		}(),
		"rename": func() error { _, err := e.svc.Rename(ctx, bob, doc.ID, "y.txt"); return err }(),
		"trash":  e.svc.Trash(ctx, bob, doc.ID),
		"reserve": func() error {
			_, err := e.svc.ReserveUpload(ctx, bob, Upload{ParentID: shared.ID, Name: "x", OnConflict: ConflictFail})
			return err
		}(),
	} {
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("read-only %s: %v, want ErrForbidden", what, err)
		}
	}

	// Write: work inside, on the owner's quota and into the owner's trash.
	if _, err := e.pool.Exec(ctx, `UPDATE shares SET perms = 3`); err != nil {
		t.Fatal(err)
	}
	usedBefore := e.usedBytes(t)
	sub, err := e.svc.CreateDir(ctx, bob, shared.ID, "from bob")
	if err != nil || sub.OwnerID != e.user || !sub.Perms.Has(access.Write) {
		t.Fatalf("mkdir with write: %+v, %v", sub, err)
	}
	f, err := e.svc.CreateFile(ctx, bob, sub.ID, "b.txt", strings.NewReader("12345"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if used := e.usedBytes(t); used != usedBefore+5 {
		t.Fatalf("owner's used %d, want %d", used, usedBefore+5)
	}
	if _, err := e.svc.Move(ctx, bob, f.ID, shared.ID, ""); err != nil {
		t.Fatalf("move inside the share: %v", err)
	}
	if err := e.svc.Trash(ctx, bob, f.ID); err != nil {
		t.Fatal(err)
	}
	if names := e.trashNames(t); len(names) != 1 || !strings.HasSuffix(names[0], "|b.txt") {
		t.Fatalf("owner's trash: %v", names)
	}
	// The shared folder itself belongs to a folder bob can't change.
	if _, err := e.svc.Rename(ctx, bob, shared.ID, "Mine"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("rename the shared folder: %v", err)
	}
	if err := e.svc.Trash(ctx, bob, shared.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("delete the shared folder: %v", err)
	}
	// Moving between owners' trees is refused, both ways.
	if _, err := e.svc.Move(ctx, bob, doc.ID, bobRoot.ID, ""); !errors.Is(err, ErrCrossOwner) {
		t.Fatalf("move out to bob's tree: %v", err)
	}
	mine, err := e.svc.CreateFile(ctx, bob, bobRoot.ID, "mine.txt", strings.NewReader("m"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Move(ctx, bob, mine.ID, shared.ID, ""); !errors.Is(err, ErrCrossOwner) {
		t.Fatalf("move in from bob's tree: %v", err)
	}
	// The trash stays the owner's: bob can't restore from it.
	if _, err := e.svc.Restore(ctx, bob, f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob restores from alice's trash: %v", err)
	}
	e.checkSizes(t)

	// Revoked: gone on the next request.
	if _, err := e.pool.Exec(ctx, `DELETE FROM shares`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Get(ctx, bob, doc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after revoke: %v", err)
	}
}
