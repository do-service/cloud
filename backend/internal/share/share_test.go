package share

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/db"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/storage/localfs"
)

type env struct {
	pool   *pgxpool.Pool
	files  *fs.Service
	shares *Service
	users  map[string]string
	root   fs.Node // alice's
}

func newEnv(t *testing.T) env {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
	}
	if err := db.Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, url, 8, 0, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE users, nodes, blobs, versions, groups, uploads CASCADE`); err != nil {
		t.Fatal(err)
	}
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	files := fs.New(pool, store)
	e := env{pool: pool, files: files, shares: New(pool, files), users: map[string]string{}}
	for _, name := range []string{"alice", "bob", "carol", "dave", "eve"} {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO users (username, display_name) VALUES ($1::text, initcap($1::text)) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		e.users[name] = id
		root, err := files.EnsureRoot(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if name == "alice" {
			e.root = root
		}
	}
	return e
}

func (e env) mkdir(t *testing.T, parent, name string) fs.Node {
	t.Helper()
	n, err := e.files.CreateDir(context.Background(), e.users["alice"], parent, name)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (e env) grant(t *testing.T, by, nodeID, to string, p access.Perm) Share {
	t.Helper()
	s, err := e.shares.Grant(context.Background(), e.users[by], nodeID, GranteeUser, e.users[to], p)
	if err != nil {
		t.Fatalf("%s grants %s: %v", by, to, err)
	}
	return s
}

func TestGrantRules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "Docs")

	s := e.grant(t, "alice", docs.ID, "bob", access.Read)
	if s.Grantee.Name != "Bob" || s.Perms != access.Read {
		t.Fatalf("share: %+v", s)
	}
	// Granting again to the same person changes their rights.
	if s2 := e.grant(t, "alice", docs.ID, "bob", access.Read|access.Reshare); s2.ID != s.ID || s2.Perms != access.Read|access.Reshare {
		t.Fatalf("regrant: %+v", s2)
	}
	for _, tc := range []struct {
		name  string
		gType string
		gID   string
		perms access.Perm
		want  error
	}{
		{"no read", GranteeUser, e.users["carol"], access.Write, ErrInvalidPerms},
		{"unknown bit", GranteeUser, e.users["carol"], access.Read | 8, ErrInvalidPerms},
		{"the owner", GranteeUser, e.users["alice"], access.Read, ErrInvalidGrantee},
		{"nobody", GranteeUser, "00000000-0000-0000-0000-000000000000", access.Read, ErrInvalidGrantee},
		{"not a uuid", GranteeUser, "x", access.Read, ErrInvalidGrantee},
		{"a link", "link", e.users["carol"], access.Read, ErrNotRecipients},
	} {
		if _, err := e.shares.Grant(ctx, e.users["alice"], docs.ID, tc.gType, tc.gID, tc.perms); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}

	// bob may reshare, but not more than he has.
	carol := e.grant(t, "bob", docs.ID, "carol", access.Read)
	if _, err := e.shares.Grant(ctx, e.users["bob"], docs.ID, GranteeUser, e.users["carol"], access.Read|access.Write); !errors.Is(err, ErrInvalidPerms) {
		t.Errorf("bob grants write: %v", err)
	}
	dave := e.grant(t, "alice", docs.ID, "dave", access.Read|access.Write)
	if _, err := e.shares.Update(ctx, e.users["bob"], dave.ID, access.Read); !errors.Is(err, fs.ErrForbidden) {
		t.Errorf("bob narrows alice's wider share: %v", err)
	}
	if err := e.shares.Revoke(ctx, e.users["bob"], dave.ID); !errors.Is(err, fs.ErrForbidden) {
		t.Errorf("bob revokes alice's wider share: %v", err)
	}
	if _, err := e.shares.Grant(ctx, e.users["bob"], docs.ID, GranteeUser, e.users["dave"], access.Read); !errors.Is(err, fs.ErrForbidden) {
		t.Errorf("bob regrants over alice's wider share: %v", err)
	}
	// carol can read but not reshare, and eve sees nothing at all.
	if _, err := e.shares.List(ctx, e.users["carol"], docs.ID); !errors.Is(err, fs.ErrForbidden) {
		t.Errorf("carol lists: %v", err)
	}
	if err := e.shares.Revoke(ctx, e.users["eve"], carol.ID); !errors.Is(err, ErrNoShare) {
		t.Errorf("eve revokes: %v", err)
	}
	// A recipient may leave a share.
	if err := e.shares.Revoke(ctx, e.users["carol"], carol.ID); err != nil {
		t.Fatalf("carol leaves: %v", err)
	}
	list, err := e.shares.List(ctx, e.users["alice"], docs.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("shares left: %+v, %v", list, err)
	}
}

func TestSharedWithMe(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.mkdir(t, e.root.ID, "A")
	b := e.mkdir(t, a.ID, "B")
	other := e.mkdir(t, e.root.ID, "Other")
	e.grant(t, "alice", a.ID, "bob", access.Read)
	e.grant(t, "alice", b.ID, "bob", access.Read|access.Write)
	team, err := e.shares.CreateGroup(ctx, "Команда")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.shares.SetMember(ctx, team.ID, e.users["bob"], true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.shares.Grant(ctx, e.users["alice"], other.ID, GranteeGroup, team.ID, access.Read); err != nil {
		t.Fatal(err)
	}

	items, err := e.shares.SharedWithMe(ctx, e.users["bob"])
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range items {
		names = append(names, it.Name)
		if it.Owner.Name != "Alice" {
			t.Errorf("owner %+v", it.Owner)
		}
	}
	// B is inside A, already shared: reached through A.
	if len(names) != 2 || !contains(names, "A") || !contains(names, "Other") {
		t.Fatalf("shared with bob: %v", names)
	}

	// Deleting the group takes its shares with it.
	if err := e.shares.DeleteGroup(ctx, team.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.files.Get(ctx, e.users["bob"], other.ID); !errors.Is(err, fs.ErrNotFound) {
		t.Fatalf("after the group is gone: %v", err)
	}
}

func TestDirectoryAndGroups(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.shares.CreateGroup(ctx, "Бухгалтерия"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.shares.CreateGroup(ctx, "бухгалтерия"); !errors.Is(err, ErrGroupExists) {
		t.Fatalf("duplicate group: %v", err)
	}
	if _, err := e.shares.CreateGroup(ctx, "  "); !errors.Is(err, ErrGroupName) {
		t.Fatalf("blank group: %v", err)
	}
	found, err := e.shares.Directory(ctx, e.users["alice"], "a", 20)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range found {
		got = append(got, g.Type+":"+g.Name)
	}
	// alice herself isn't offered; "a" matches carol and dave.
	if contains(got, "user:Alice") || !contains(got, "user:Carol") || !contains(got, "user:Dave") {
		t.Fatalf("directory: %v", got)
	}
	if found, err := e.shares.Directory(ctx, e.users["alice"], "БУХ", 20); err != nil || len(found) != 1 || found[0].Type != GranteeGroup {
		t.Fatalf("directory finds groups, ignoring case: %+v, %v", found, err)
	}
	groups, err := e.shares.Groups(ctx)
	if err != nil || len(groups) != 1 || len(groups[0].Members) != 0 {
		t.Fatalf("groups: %+v, %v", groups, err)
	}
	if err := e.shares.SetMember(ctx, groups[0].ID, "00000000-0000-0000-0000-000000000000", true); !errors.Is(err, ErrInvalidGrantee) {
		t.Fatalf("member nobody: %v", err)
	}
	if err := e.shares.RenameGroup(ctx, "00000000-0000-0000-0000-000000000000", "x"); !errors.Is(err, ErrNoGroup) {
		t.Fatalf("rename missing: %v", err)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
