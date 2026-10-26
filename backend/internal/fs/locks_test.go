package fs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/do-service/cloud/backend/internal/access"
)

func (e env) lock(t *testing.T, actorID, id string, deep bool) Lock {
	t.Helper()
	l, err := e.svc.Lock(context.Background(), actorID, id, LockRequest{Kind: LockWebDAV, Deep: deep, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("lock %s: %v", id, err)
	}
	return l
}

func wantLocked(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("%s: err = %v, want ErrLocked", what, err)
	}
}

// Every write is refused on a locked node, through whatever protocol,
// unless its owner presents the token.
func TestLockedFile(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "docs")
	f := e.file(t, docs.ID, "a.txt", "a")
	l := e.lock(t, e.user, f.ID, false)
	if !strings.HasPrefix(l.Token, "opaquelocktoken:") || time.Until(l.ExpiresAt) > time.Minute {
		t.Fatalf("lock = %+v", l)
	}

	_, _, err := e.svc.PutFile(ctx, e.user, docs.ID, "a.txt", strings.NewReader("b"), 1, PutCond{})
	wantLocked(t, "put without the token", err)
	_, err = e.svc.Rename(ctx, e.user, f.ID, "b.txt")
	wantLocked(t, "rename", err)
	wantLocked(t, "trash", e.svc.Trash(ctx, e.user, f.ID))
	wantLocked(t, "props", e.svc.PatchProps(ctx, e.user, f.ID, []PropChange{{Prop: Prop{Space: "urn:x", Local: "p", Value: "v"}}}))
	_, err = e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: docs.ID, Name: "A.TXT", Size: 1, OnConflict: ConflictReplace})
	wantLocked(t, "replacing upload", err)
	// Its folder isn't locked: other files come and go.
	e.file(t, docs.ID, "b.txt", "b")
	if _, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: docs.ID, Name: "a.txt", Size: 1, OnConflict: ConflictRename}); err != nil {
		t.Fatalf("upload beside: %v", err)
	}

	held := WithLockTokens(ctx, []string{l.Token})
	if _, _, err := e.svc.PutFile(held, e.user, docs.ID, "a.txt", strings.NewReader("b"), 1, PutCond{}); err != nil {
		t.Fatalf("put with the token: %v", err)
	}
	// Someone else with write access can't use the owner's token.
	bob := e.newUser(t, "bob")
	e.share(t, docs.ID, bob, access.Read|access.Write)
	_, _, err = e.svc.PutFile(held, bob, docs.ID, "a.txt", strings.NewReader("c"), 1, PutCond{})
	wantLocked(t, "put with another's token", err)
	if _, err := e.svc.Lock(ctx, bob, f.ID, LockRequest{Kind: LockWebDAV}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	if err := e.svc.Unlock(ctx, bob, l.Token); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unlock by another: %v", err)
	}
	if _, err := e.svc.RefreshLock(ctx, bob, l.Token, time.Minute); !errors.Is(err, ErrForbidden) {
		t.Fatalf("refresh by another: %v", err)
	}

	if err := e.svc.Unlock(ctx, e.user, l.Token); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Unlock(ctx, e.user, l.Token); !errors.Is(err, ErrNoLock) {
		t.Fatalf("second unlock: %v", err)
	}
	if _, err := e.svc.Rename(ctx, e.user, f.ID, "b2.txt"); err != nil {
		t.Fatalf("rename after unlock: %v", err)
	}
}

func TestLockedFolder(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "docs")
	sub := e.mkdir(t, docs.ID, "sub")
	f := e.file(t, sub.ID, "a.txt", "a")
	other := e.mkdir(t, e.root.ID, "other")

	// A shallow lock guards the folder and its members, not deeper.
	shallow := e.lock(t, e.user, docs.ID, false)
	_, err := e.svc.CreateDir(ctx, e.user, docs.ID, "new")
	wantLocked(t, "add a member", err)
	_, err = e.svc.Move(ctx, e.user, sub.ID, other.ID, "")
	wantLocked(t, "move a member out", err)
	if _, err := e.svc.Rename(ctx, e.user, f.ID, "b.txt"); err != nil {
		t.Fatalf("deeper is free: %v", err)
	}
	if err := e.svc.Unlock(ctx, e.user, shallow.Token); err != nil {
		t.Fatal(err)
	}

	// A deep one covers everything below.
	deep := e.lock(t, e.user, docs.ID, true)
	_, err = e.svc.Rename(ctx, e.user, f.ID, "c.txt")
	wantLocked(t, "rename below a deep lock", err)
	if _, err := e.svc.Lock(ctx, e.user, f.ID, LockRequest{Kind: LockWebDAV}); !errors.Is(err, ErrLocked) {
		t.Fatalf("lock below a deep lock: %v", err)
	}
	locks, err := e.svc.Locks(ctx, e.user, sub.ID, true)
	if err != nil || len(locks[sub.ID]) != 1 || locks[sub.ID][0].Above != 1 || len(locks[f.ID]) != 1 || locks[f.ID][0].Above != 2 {
		t.Fatalf("locks = %+v, %v", locks, err)
	}
	if err := e.svc.Unlock(ctx, e.user, deep.Token); err != nil {
		t.Fatal(err)
	}

	// A locked file inside keeps the folder from moving or going away,
	// and from being locked deep over it.
	l := e.lock(t, e.user, f.ID, false)
	_, err = e.svc.Move(ctx, e.user, docs.ID, other.ID, "")
	wantLocked(t, "move the folder", err)
	wantLocked(t, "trash the folder", e.svc.Trash(ctx, e.user, docs.ID))
	if _, err := e.svc.Lock(ctx, e.user, docs.ID, LockRequest{Kind: LockWebDAV, Deep: true}); !errors.Is(err, ErrLocked) {
		t.Fatalf("deep lock over a lock: %v", err)
	}
	if _, err := e.svc.Move(WithLockTokens(ctx, []string{l.Token}), e.user, docs.ID, other.ID, ""); err != nil {
		t.Fatalf("move with the token: %v", err)
	}
}

func TestLockExpiry(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	f := e.file(t, e.root.ID, "a.txt", "a")
	l, err := e.svc.Lock(ctx, e.user, f.ID, LockRequest{Kind: LockWebDAV, Timeout: 365 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(l.ExpiresAt) > MaxLockTimeout {
		t.Fatalf("timeout not capped: %v", l.ExpiresAt)
	}
	if l, err = e.svc.RefreshLock(ctx, e.user, l.Token, 0); err != nil || time.Until(l.ExpiresAt) < MaxLockTimeout-time.Minute {
		t.Fatalf("refresh: %+v, %v", l, err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE locks SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	// An expired lock holds nothing, and a new one takes its place.
	if _, err := e.svc.Rename(ctx, e.user, f.ID, "b.txt"); err != nil {
		t.Fatalf("rename under an expired lock: %v", err)
	}
	if _, err := e.svc.RefreshLock(ctx, e.user, l.Token, time.Minute); !errors.Is(err, ErrNoLock) {
		t.Fatalf("refresh expired: %v", err)
	}
	e.lock(t, e.user, f.ID, false)
}
