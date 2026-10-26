package share

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
)

func (e env) links(t *testing.T) *Links {
	t.Helper()
	box, err := auth.NewSecretBox([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return NewLinks(e.shares, box)
}

func TestLinks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	links := e.links(t)
	alice, bob := e.users["alice"], e.users["bob"]
	docs := e.mkdir(t, e.root.ID, "Docs")
	f, err := e.files.CreateFile(ctx, alice, docs.ID, "a.txt", strings.NewReader("a"), 1)
	if err != nil {
		t.Fatal(err)
	}

	noKey, err := auth.NewSecretBox(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLinks(e.shares, noKey).Create(ctx, alice, docs.ID, ModeView, "", nil); !errors.Is(err, ErrLinksDisabled) {
		t.Fatalf("without SECRET_KEY: %v", err)
	}
	for _, tc := range []struct {
		name, node, mode string
	}{{"unknown mode", docs.ID, "edit"}, {"upload to a file", f.ID, ModeUpload}} {
		if _, err := links.Create(ctx, alice, tc.node, tc.mode, "", nil); !errors.Is(err, ErrInvalidMode) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}

	l, err := links.Create(ctx, alice, docs.ID, ModeView, "", nil)
	if err != nil || len(l.Token) < 43 {
		t.Fatalf("create: %+v, %v", l, err)
	}
	// The owner can copy it again later.
	if list, err := links.List(ctx, alice, docs.ID); err != nil || len(list) != 1 || list[0].Token != l.Token {
		t.Fatalf("list: %+v, %v", list, err)
	}
	p, err := links.Resolve(ctx, l.Token)
	if err != nil || p.NodeID != docs.ID || p.Name != "Docs" || p.Mode != ModeView || p.HasPassword {
		t.Fatalf("resolve: %+v, %v", p, err)
	}
	if ok, err := links.Unlocked(ctx, p, ""); err != nil || !ok {
		t.Fatalf("no password, unlocked: %v, %v", ok, err)
	}
	if _, err := links.Resolve(ctx, l.Token+"x"); !errors.Is(err, ErrNoLink) {
		t.Fatalf("wrong token: %v", err)
	}

	// Through the link: its subtree, read only, nothing above.
	if n, err := e.files.Get(ctx, p.Principal(), f.ID); err != nil || n.Perms != access.Read {
		t.Fatalf("read through link: %+v, %v", n.Perms, err)
	}
	if _, err := e.files.Get(ctx, p.Principal(), e.root.ID); !errors.Is(err, fs.ErrNotFound) {
		t.Fatalf("above the link: %v", err)
	}
	if _, err := e.files.CreateDir(ctx, p.Principal(), docs.ID, "x"); !errors.Is(err, fs.ErrForbidden) {
		t.Fatalf("write through a view link: %v", err)
	}
	if path, err := e.files.Path(ctx, p.Principal(), f.ID); err != nil || len(path) != 2 || path[0].ID != docs.ID {
		t.Fatalf("path through link: %+v, %v", path, err)
	}

	// Password: wrong, right, then changed (old sessions end).
	pw := "секрет"
	if _, err := links.Update(ctx, alice, l.ID, LinkChange{Password: &pw}); err != nil {
		t.Fatal(err)
	}
	p, _ = links.Resolve(ctx, l.Token)
	if ok, _ := links.Unlocked(ctx, p, ""); ok || !p.HasPassword {
		t.Fatal("password not asked")
	}
	if _, _, err := links.Unlock(ctx, p, "nope"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	session, _, err := links.Unlock(ctx, p, pw)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := links.Unlocked(ctx, p, session); !ok {
		t.Fatal("session not accepted")
	}
	other := "другой"
	if _, err := links.Update(ctx, alice, l.ID, LinkChange{Password: &other}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := links.Unlocked(ctx, p, session); ok {
		t.Fatal("old session survived a password change")
	}

	// Expired, and the node trashed: both make the token unknown.
	past := time.Now().Add(-time.Minute)
	pastPtr := &past
	if _, err := links.Update(ctx, alice, l.ID, LinkChange{ExpiresAt: &pastPtr}); err != nil {
		t.Fatal(err)
	}
	if _, err := links.Resolve(ctx, l.Token); !errors.Is(err, ErrNoLink) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := e.files.Get(ctx, p.Principal(), f.ID); !errors.Is(err, fs.ErrNotFound) {
		t.Fatalf("expired principal still reads: %v", err)
	}

	// Upload: write without read, on the owner's quota.
	up, err := links.Create(ctx, alice, docs.ID, ModeUpload, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	upP, _ := links.Resolve(ctx, up.Token)
	if _, _, err := e.files.List(ctx, upP.Principal(), docs.ID, fs.ListOptions{}); !errors.Is(err, fs.ErrForbidden) {
		t.Fatalf("list through an upload link: %v", err)
	}
	dropped, err := e.files.CreateFile(ctx, upP.Principal(), docs.ID, "dropped.txt", strings.NewReader("hi"), 2)
	if err != nil || dropped.OwnerID != alice {
		t.Fatalf("drop a file: %+v, %v", dropped, err)
	}
	// Whoever dropped it isn't a user: the content has no author.
	if _, author, err := e.files.ListVersions(ctx, alice, dropped.ID); err != nil || author != (fs.Author{}) {
		t.Fatalf("dropped file's author: %+v, %v", author, err)
	}
	if err := e.files.Trash(ctx, alice, docs.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := links.Resolve(ctx, up.Token); !errors.Is(err, ErrNoLink) {
		t.Fatalf("link to a trashed folder: %v", err)
	}
	if _, err := e.files.Restore(ctx, alice, docs.ID); err != nil {
		t.Fatal(err)
	}

	// Who may manage links: the owner and resharers, no one else; revoked
	// is gone at once.
	if _, err := links.Create(ctx, bob, docs.ID, ModeView, "", nil); !errors.Is(err, fs.ErrNotFound) {
		t.Fatalf("stranger creates: %v", err)
	}
	e.grant(t, "alice", docs.ID, "bob", access.Read|access.Reshare)
	if _, err := links.Create(ctx, bob, docs.ID, ModeUpload, "", nil); !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("resharer without write makes an upload link: %v", err)
	}
	if _, err := links.Create(ctx, bob, docs.ID, ModeDownload, "", nil); err != nil {
		t.Fatalf("resharer creates: %v", err)
	}
	if err := links.Delete(ctx, e.users["carol"], up.ID); !errors.Is(err, ErrNoLink) {
		t.Fatalf("stranger deletes: %v", err)
	}
	if err := links.Delete(ctx, alice, up.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := links.Resolve(ctx, up.Token); !errors.Is(err, ErrNoLink) {
		t.Fatalf("after delete: %v", err)
	}
}

// After SECRET_KEY changes, old tokens can't be unsealed: their links are
// listed without an address and can still be revoked, new ones work.
func TestLinksAfterKeyChange(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.users["alice"]
	docs := e.mkdir(t, e.root.ID, "Docs")
	old, err := e.links(t).Create(ctx, alice, docs.ID, ModeView, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	box, err := auth.NewSecretBox([]byte(strings.Repeat("n", 32)))
	if err != nil {
		t.Fatal(err)
	}
	links := NewLinks(e.shares, box)
	fresh, err := links.Create(ctx, alice, docs.ID, ModeDownload, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	list, err := links.List(ctx, alice, docs.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %+v, %v", list, err)
	}
	if list[0].ID != old.ID || list[0].Token != "" || list[0].Mode != ModeView {
		t.Fatalf("old link: %+v", list[0])
	}
	if list[1].ID != fresh.ID || list[1].Token != fresh.Token {
		t.Fatalf("new link: %+v", list[1])
	}
	// The old URL still works until revoked.
	if _, err := links.Resolve(ctx, old.Token); err != nil {
		t.Fatalf("resolve old: %v", err)
	}
	mode := ModeDownload
	if l, err := links.Update(ctx, alice, old.ID, LinkChange{Mode: &mode}); err != nil || l.Mode != ModeDownload || l.Token != "" {
		t.Fatalf("update old: %+v, %v", l, err)
	}
	if err := links.Delete(ctx, alice, old.ID); err != nil {
		t.Fatalf("delete old: %v", err)
	}
	if _, err := links.Resolve(ctx, old.Token); !errors.Is(err, ErrNoLink) {
		t.Fatalf("resolve revoked: %v", err)
	}
	if list, err := links.List(ctx, alice, docs.ID); err != nil || len(list) != 1 || list[0].ID != fresh.ID {
		t.Fatalf("list after revoke: %+v, %v", list, err)
	}
}
