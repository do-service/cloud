package sync

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/db"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/share"
	"github.com/do-service/cloud/backend/internal/storage/localfs"
)

func testDatabaseURL() string {
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
}

type env struct {
	mux    *http.ServeMux
	files  *fs.Service
	shares *share.Service
	pool   *pgxpool.Pool
	hub    *Hub
	// stopHub shuts the hub down, as a server shutdown does.
	stopHub context.CancelFunc
	// ids and sessions by username: alice, bob, carol.
	ids      map[string]string
	sessions map[string]string
	roots    map[string]fs.Node
}

func newEnv(t *testing.T) env {
	t.Helper()
	url := testDatabaseURL()
	if err := db.Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, url, 16, 0, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE users, nodes, blobs, versions, groups CASCADE`); err != nil {
		t.Fatal(err)
	}
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	files := fs.New(pool, store)
	hubCtx, stopHub := context.WithCancel(ctx)
	t.Cleanup(stopHub)
	hub := NewHub(files)
	go hub.Run(hubCtx)
	e := env{mux: http.NewServeMux(), files: files, shares: share.New(pool, files), pool: pool, hub: hub, stopHub: stopHub,
		ids: map[string]string{}, sessions: map[string]string{}, roots: map[string]fs.Node{}}
	sessions := auth.NewSessions(pool, time.Hour)
	for _, name := range []string{"alice", "bob", "carol"} {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO users (username) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		e.ids[name] = id
		if e.roots[name], err = files.EnsureRoot(ctx, id); err != nil {
			t.Fatal(err)
		}
		if e.sessions[name], _, err = sessions.Create(ctx, id, netip.Addr{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	withUser := auth.Middleware(sessions, nil, auth.Cookies{})
	h := &Handlers{FS: files, Shares: e.shares, Hub: hub, JournalRetention: 720 * time.Hour, SignedIn: func(next http.Handler) http.Handler { return withUser(auth.RequireUser(next)) }}
	h.Register(e.mux)
	return e
}

func (e env) do(user, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	if user != "" {
		req.AddCookie(&http.Cookie{Name: "session", Value: e.sessions[user]})
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e env) rootsOf(t *testing.T, user string) rootsResponse {
	t.Helper()
	rec := e.do(user, "/api/v1/sync/roots")
	var resp rootsResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		t.Fatalf("roots of %s: %d %s", user, rec.Code, rec.Body)
	}
	return resp
}

// treeOf reads /sync/tree, checking the stream's shape on the way.
func (e env) treeOf(t *testing.T, user, root string) (string, []nodeJSON) {
	t.Helper()
	rec := e.do(user, "/api/v1/sync/tree?root="+root)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("tree %s as %s: %d %s", root, user, rec.Code, rec.Body)
	}
	sc := bufio.NewScanner(rec.Body)
	var first cursorLine
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &first) != nil || first.Cursor == "" {
		t.Fatalf("tree: no cursor line in %s", rec.Body)
	}
	var nodes []nodeJSON
	seen := map[string]bool{}
	for sc.Scan() {
		var n nodeJSON
		if err := json.Unmarshal(sc.Bytes(), &n); err != nil {
			t.Fatalf("tree line %q: %v", sc.Text(), err)
		}
		if len(nodes) > 0 && (n.ParentID == nil || !seen[*n.ParentID]) {
			t.Fatalf("tree: %s comes before its folder", n.Name)
		}
		seen[n.ID] = true
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 || nodes[0].ID != root && root != rootAlias {
		t.Fatalf("tree: root isn't first: %+v", nodes)
	}
	return first.Cursor, nodes
}

func (e env) changesOf(t *testing.T, user, cursor string, limit int) changesResponse {
	t.Helper()
	target := "/api/v1/sync/changes?cursor=" + url.QueryEscape(cursor)
	if limit > 0 {
		target += fmt.Sprintf("&limit=%d", limit)
	}
	rec := e.do(user, target)
	var resp changesResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		t.Fatalf("changes as %s: %d %s", user, rec.Code, rec.Body)
	}
	return resp
}

func (e env) mkdir(t *testing.T, user, parent, name string) fs.Node {
	t.Helper()
	n, err := e.files.CreateDir(context.Background(), e.ids[user], parent, name)
	if err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	return n
}

func (e env) file(t *testing.T, user, parent, name, content string) fs.Node {
	t.Helper()
	n, err := e.files.CreateFile(context.Background(), e.ids[user], parent, name, strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return n
}

func (e env) grant(t *testing.T, node, user string, p access.Perm) string {
	t.Helper()
	s, err := e.shares.Grant(context.Background(), e.ids["alice"], node, share.GranteeUser, e.ids[user], p)
	if err != nil {
		t.Fatal(err)
	}
	return s.ID
}

// must fails the test on an fs call's error.
func must(t *testing.T) func(fs.Node, error) {
	return func(_ fs.Node, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func byID(nodes []nodeJSON) map[string]nodeJSON {
	m := map[string]nodeJSON{}
	for _, n := range nodes {
		m[n.ID] = n
	}
	return m
}

func TestInfo(t *testing.T) {
	e := newEnv(t)
	rec := e.do("", "/api/v1/sync/info")
	var info infoJSON
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &info) != nil {
		t.Fatalf("info: %d %s", rec.Code, rec.Body)
	}
	if info.APIVersion != 1 || info.MaxWait != 55 || info.JournalRetention != 30*24*3600 || info.ServerVersion == "" || info.MinClientVersion == "" {
		t.Fatalf("info: %s", rec.Body)
	}
	for _, path := range []string{"/api/v1/sync/roots", "/api/v1/sync/tree?root=root", "/api/v1/sync/changes?cursor=x"} {
		if rec := e.do("", path); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s signed out: %d", path, rec.Code)
		}
	}
}

// Alice's tree, Shared/ shared with Bob to read and Shared/Inner/ to
// write too; Carol's own folder named Shared as well.
func TestRootsAndTree(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.roots["alice"]
	docs := e.mkdir(t, "alice", root.ID, "Docs")
	a := e.file(t, "alice", docs.ID, "a.txt", "a")
	sub := e.mkdir(t, "alice", docs.ID, "Sub")
	e.file(t, "alice", sub.ID, "b.txt", "bb")
	shared := e.mkdir(t, "alice", root.ID, "Shared")
	c := e.file(t, "alice", shared.ID, "c.txt", "ccc")
	inner := e.mkdir(t, "alice", shared.ID, "Inner")
	d := e.file(t, "alice", inner.ID, "d.txt", "dddd")
	gone := e.file(t, "alice", shared.ID, "gone.txt", "x")
	if err := e.files.Trash(ctx, e.ids["alice"], gone.ID); err != nil {
		t.Fatal(err)
	}
	e.grant(t, shared.ID, "bob", access.Read)
	e.grant(t, inner.ID, "bob", access.Read|access.Write)
	carols := e.mkdir(t, "carol", e.roots["carol"].ID, "Shared")
	if _, err := e.shares.Grant(ctx, e.ids["carol"], carols.ID, share.GranteeUser, e.ids["bob"], access.Read); err != nil {
		t.Fatal(err)
	}

	// Alice: her tree, all of it but the trash.
	_, nodes := e.treeOf(t, "alice", rootAlias)
	if len(nodes) != 9 || nodes[0].ID != root.ID || nodes[0].ParentID != nil {
		t.Fatalf("alice's tree: %+v", nodes)
	}
	all := byID(nodes)
	if _, ok := all[gone.ID]; ok {
		t.Error("trashed file in the tree")
	}
	sum := sha256.Sum256([]byte("dddd"))
	if n := all[d.ID]; n.SHA256 != hex.EncodeToString(sum[:]) || *n.ParentID != inner.ID || n.Size != 4 || !slices.Equal(n.Permissions, []string{"read", "write", "reshare"}) {
		t.Errorf("d.txt: %+v", n)
	}
	if n := all[docs.ID]; n.SHA256 != "" || n.Kind != fs.KindDir || n.ETag == "" {
		t.Errorf("Docs: %+v", n)
	}

	// Bob: his root and the two shared, names told apart by owner.
	roots := e.rootsOf(t, "bob")
	if len(roots.Roots) != 3 || roots.Roots[0].Kind != "own" || roots.Roots[0].ID != e.roots["bob"].ID || roots.Cursor == "" {
		t.Fatalf("bob's roots: %+v", roots)
	}
	names := map[string]rootJSON{}
	for _, r := range roots.Roots[1:] {
		names[r.Name] = r
	}
	if r := names["Shared (alice)"]; r.ID != shared.ID || r.Kind != "shared" || r.Owner.ID != e.ids["alice"] || !slices.Equal(r.Permissions, []string{"read"}) {
		t.Errorf("alice's share: %+v", roots.Roots)
	}
	if r := names["Shared (carol)"]; r.ID != carols.ID || r.Owner.Name != "carol" {
		t.Errorf("carol's share: %+v", roots.Roots)
	}

	// Bob's tree of Shared: from it down, rights as shared.
	_, nodes = e.treeOf(t, "bob", shared.ID)
	got := byID(nodes)
	if len(nodes) != 4 || nodes[0].ParentID != nil {
		t.Fatalf("bob's tree: %+v", nodes)
	}
	if !slices.Equal(got[c.ID].Permissions, []string{"read"}) || !slices.Equal(got[inner.ID].Permissions, []string{"read", "write"}) ||
		!slices.Equal(got[d.ID].Permissions, []string{"read", "write"}) {
		t.Errorf("bob's rights: %+v", nodes)
	}
	// Inner is below Shared, which he sees: its parent is told.
	_, nodes = e.treeOf(t, "bob", inner.ID)
	if nodes[0].ParentID == nil || *nodes[0].ParentID != shared.ID {
		t.Errorf("Inner as a tree root: %+v", nodes[0])
	}

	for _, tc := range []struct {
		user, path string
		code       int
	}{
		{"bob", "/api/v1/sync/tree?root=" + root.ID, http.StatusNotFound},
		{"bob", "/api/v1/sync/tree?root=" + docs.ID, http.StatusNotFound},
		{"carol", "/api/v1/sync/tree?root=" + shared.ID, http.StatusNotFound},
		{"carol", "/api/v1/sync/tree?root=nope", http.StatusNotFound},
		{"carol", "/api/v1/sync/tree", http.StatusBadRequest},
	} {
		if rec := e.do(tc.user, tc.path); rec.Code != tc.code {
			t.Errorf("%s as %s: %d %s", tc.path, tc.user, rec.Code, rec.Body)
		}
	}
	// A shared file is a tree of one.
	if _, err := e.shares.Grant(ctx, e.ids["alice"], a.ID, share.GranteeUser, e.ids["carol"], access.Read); err != nil {
		t.Fatal(err)
	}
	if _, nodes := e.treeOf(t, "carol", a.ID); len(nodes) != 1 || nodes[0].ParentID != nil || nodes[0].Kind != fs.KindFile {
		t.Errorf("carol's tree of a.txt: %+v", nodes)
	}
}

func ids(changes []changeJSON) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = c.ID
	}
	return out
}

func TestChangesOwn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.ids["alice"]
	root := e.roots["alice"]
	start := e.rootsOf(t, "alice").Cursor

	if resp := e.changesOf(t, "alice", start, 0); len(resp.Changes) != 0 || resp.More || resp.RootsChanged || resp.Cursor != start {
		t.Fatalf("nothing happened: %+v", resp)
	}
	f := e.file(t, "alice", root.ID, "f.txt", "1")
	dir := e.mkdir(t, "alice", root.ID, "Dir")
	// Moved into a folder created after it: the folder still comes first.
	must(t)(e.files.Move(ctx, alice, f.ID, dir.ID, ""))
	must(t)(e.files.Rename(ctx, alice, f.ID, "g.txt"))
	gone := e.file(t, "alice", root.ID, "gone.txt", "x")
	if err := e.files.Trash(ctx, alice, gone.ID); err != nil {
		t.Fatal(err)
	}

	resp := e.changesOf(t, "alice", start, 0)
	if !slices.Equal(ids(resp.Changes), []string{dir.ID, f.ID, gone.ID}) || resp.More {
		t.Fatalf("changes: %+v", resp)
	}
	if c := resp.Changes[1]; c.Deleted || c.Node == nil || c.Node.Name != "g.txt" || *c.Node.ParentID != dir.ID || c.Owner != alice || c.Node.SHA256 == "" {
		t.Errorf("f.txt: %+v", c)
	}
	if c := resp.Changes[2]; !c.Deleted || c.Node != nil || c.Owner != alice {
		t.Errorf("tombstone: %+v", c)
	}
	// Paging: a row at a time gives the same, a page each.
	var paged []string
	for cur, pages := start, 0; ; pages++ {
		r := e.changesOf(t, "alice", cur, 1)
		paged = append(paged, ids(r.Changes)...)
		cur = r.Cursor
		if !r.More {
			if pages != 5 || cur != resp.Cursor {
				t.Errorf("paging: %d pages, cursor %s, want %s", pages, cur, resp.Cursor)
			}
			break
		}
	}
	if !slices.Equal(paged, []string{f.ID, dir.ID, f.ID, f.ID, gone.ID, gone.ID}) {
		t.Errorf("paged: %v", paged)
	}

	// Trash and restore a folder: back, with its contents to read again.
	cur := resp.Cursor
	if err := e.files.Trash(ctx, alice, dir.ID); err != nil {
		t.Fatal(err)
	}
	if r := e.changesOf(t, "alice", cur, 0); len(r.Changes) != 1 || !r.Changes[0].Deleted {
		t.Fatalf("trashed folder: %+v", r)
	}
	must(t)(e.files.Restore(ctx, alice, dir.ID))
	r := e.changesOf(t, "alice", cur, 0)
	if len(r.Changes) != 1 || r.Changes[0].Deleted || !r.Changes[0].Subtree {
		t.Fatalf("restored folder: %+v", r)
	}
	// Purged: the trashing's tombstone stands.
	cur = r.Cursor
	if err := e.files.Trash(ctx, alice, dir.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.files.Purge(ctx, alice, dir.ID); err != nil {
		t.Fatal(err)
	}
	if r := e.changesOf(t, "alice", cur, 0); len(r.Changes) != 1 || !r.Changes[0].Deleted || r.Changes[0].ID != dir.ID {
		t.Fatalf("purged folder: %+v", r)
	}

	// Alice's changes are hers alone: Bob's cursor sees none of them.
	bobs := e.rootsOf(t, "bob").Cursor
	e.file(t, "alice", root.ID, "private.txt", "p")
	if r := e.changesOf(t, "bob", bobs, 0); len(r.Changes) != 0 {
		t.Errorf("bob sees alice's changes: %+v", r)
	}
}

func TestChangesCursor(t *testing.T) {
	e := newEnv(t)
	e.file(t, "alice", e.roots["alice"].ID, "f.txt", "1")
	good := e.rootsOf(t, "alice").Cursor
	c, err := decodeCursor(good)
	if err != nil {
		t.Fatal(err)
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []string{
		"", "x", "!!!", enc(`[]`), enc(`{}`),
		enc(`{"v":2,"own":0,"a":0,"s":{},"r":"` + c.R + `"}`),
		enc(`{"v":1,"own":-1,"a":0,"s":{},"r":"` + c.R + `"}`),
		enc(`{"v":1,"own":0,"a":0,"s":{"nope":1},"r":"` + c.R + `"}`),
		enc(`{"v":1,"own":0,"a":0,"s":{},"r":"` + c.R + `","x":1}`),
		enc(`{"v":1,"own":0,"a":0,"s":{},"r":"zz"}`),
		enc(`{"v":1,"own":0,"a":0,"s":{},"r":"` + c.R + `"} {}`),
	} {
		rec := e.do("alice", "/api/v1/sync/changes?cursor="+url.QueryEscape(bad))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_request"`) {
			t.Errorf("cursor %q: %d %s", bad, rec.Code, rec.Body)
		}
	}
	for _, q := range []string{"&limit=0", "&limit=1001", "&limit=x", "&wait=56", "&wait=-1"} {
		if rec := e.do("alice", "/api/v1/sync/changes?cursor="+good+q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
	// From before f.txt: with a change to report, a long poll answers at
	// once (TestLongPoll has the waiting).
	before := c
	before.Own = 0
	if rec := e.do("alice", "/api/v1/sync/changes?cursor="+before.encode()+"&wait=55"); rec.Code != http.StatusOK {
		t.Errorf("wait=55: %d", rec.Code)
	}
	// Ahead of the journal (a restored backup): can't carry on.
	ahead := c
	ahead.Own = 99
	if rec := e.do("alice", "/api/v1/sync/changes?cursor="+ahead.encode()); rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), `"cursor_expired"`) {
		t.Errorf("ahead: %d %s", rec.Code, rec.Body)
	}
	// Retention moved past it.
	old := c
	old.Own = 0
	if _, err := e.pool.Exec(context.Background(), `UPDATE sync_heads SET min_seq = 1 WHERE user_id = $1`, e.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if rec := e.do("alice", "/api/v1/sync/changes?cursor="+old.encode()); rec.Code != http.StatusGone {
		t.Errorf("expired: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("alice", "/api/v1/sync/changes?cursor="+good); rec.Code != http.StatusOK {
		t.Errorf("at min_seq: %d %s", rec.Code, rec.Body)
	}
}

// Bob follows Alice's Shared: what happens in it, what comes into it and
// leaves it, the share itself going.
func TestChangesShared(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.ids["alice"]
	root := e.roots["alice"]
	docs := e.mkdir(t, "alice", root.ID, "Docs")
	shared := e.mkdir(t, "alice", root.ID, "Shared")
	moving := e.file(t, "alice", shared.ID, "moving.txt", "m")
	grant := e.grant(t, shared.ID, "bob", access.Read)
	bob := e.rootsOf(t, "bob")
	c, err := decodeCursor(bob.Cursor)
	if err != nil || c.S[alice] == 0 {
		t.Fatalf("bob's cursor doesn't follow alice: %+v %v", c, err)
	}

	in := e.file(t, "alice", shared.ID, "in.txt", "i")
	e.file(t, "alice", docs.ID, "outside.txt", "o")
	outDir := e.mkdir(t, "alice", docs.ID, "Out")
	e.file(t, "alice", outDir.ID, "deep.txt", "d")
	must(t)(e.files.Move(ctx, alice, moving.ID, docs.ID, ""))
	must(t)(e.files.Move(ctx, alice, outDir.ID, shared.ID, ""))
	must(t)(e.files.Rename(ctx, alice, shared.ID, "Shared2"))

	resp := e.changesOf(t, "bob", bob.Cursor, 0)
	if !slices.Equal(ids(resp.Changes), []string{shared.ID, in.ID, outDir.ID, moving.ID}) || resp.RootsChanged {
		t.Fatalf("bob's changes: %+v", resp)
	}
	if c := resp.Changes[2]; !c.Subtree || c.Node == nil || *c.Node.ParentID != shared.ID || c.Owner != alice {
		t.Errorf("folder moved in: %+v", c)
	}
	if c := resp.Changes[0]; c.Node == nil || c.Node.Name != "Shared2" || c.Node.ParentID != nil {
		t.Errorf("the shared root, renamed: %+v", c.Node)
	}
	if c := resp.Changes[3]; !c.Deleted {
		t.Errorf("file moved out: %+v", c)
	}
	// Carol, with nothing shared, sees none of it.
	if r := e.changesOf(t, "carol", e.rootsOf(t, "carol").Cursor, 0); len(r.Changes) != 0 {
		t.Errorf("carol: %+v", r)
	}

	// The root itself trashed and back: a tombstone, then a subtree.
	cur := resp.Cursor
	if err := e.files.Trash(ctx, alice, shared.ID); err != nil {
		t.Fatal(err)
	}
	if r := e.changesOf(t, "bob", cur, 0); len(r.Changes) != 1 || !r.Changes[0].Deleted || r.Changes[0].ID != shared.ID {
		t.Fatalf("shared root trashed: %+v", r)
	}
	must(t)(e.files.Restore(ctx, alice, shared.ID))
	r := e.changesOf(t, "bob", cur, 0)
	if len(r.Changes) != 1 || !r.Changes[0].Subtree {
		t.Fatalf("shared root restored: %+v", r)
	}

	// Revoked: the roots changed, and the tree drops out of the cursor;
	// changes made since don't reach him.
	cur = r.Cursor
	if err := e.shares.Revoke(ctx, alice, grant); err != nil {
		t.Fatal(err)
	}
	e.file(t, "alice", shared.ID, "after.txt", "a")
	r = e.changesOf(t, "bob", cur, 0)
	if !r.RootsChanged || len(r.Changes) != 0 {
		t.Fatalf("revoked: %+v", r)
	}
	if c, _ := decodeCursor(r.Cursor); len(c.S) != 0 {
		t.Errorf("cursor still follows alice: %+v", c)
	}
	if roots := e.rootsOf(t, "bob"); len(roots.Roots) != 1 {
		t.Errorf("roots after revoke: %+v", roots)
	}
	if r := e.changesOf(t, "bob", r.Cursor, 0); r.RootsChanged {
		t.Errorf("roots changed again: %+v", r)
	}

	// Of two shares, one revoked, the other's subtree still followed;
	// a file moved from the revoked one into it comes as itself.
	keep := e.mkdir(t, "alice", root.ID, "Keep")
	e.grant(t, keep.ID, "bob", access.Read)
	lose := e.mkdir(t, "alice", root.ID, "Lose")
	lost := e.grant(t, lose.ID, "bob", access.Read)
	f := e.file(t, "alice", lose.ID, "f.txt", "f")
	cur = e.rootsOf(t, "bob").Cursor
	if err := e.shares.Revoke(ctx, alice, lost); err != nil {
		t.Fatal(err)
	}
	e.file(t, "alice", lose.ID, "unseen.txt", "u")
	must(t)(e.files.Move(ctx, alice, f.ID, keep.ID, ""))
	r = e.changesOf(t, "bob", cur, 0)
	if !r.RootsChanged || !slices.Equal(ids(r.Changes), []string{f.ID}) || r.Changes[0].Deleted {
		t.Errorf("one of two revoked: %+v", r)
	}
	// A new share: the roots changed, the tree starts at its head.
	cur = r.Cursor
	carols := e.mkdir(t, "carol", e.roots["carol"].ID, "C")
	if _, err := e.shares.Grant(ctx, e.ids["carol"], carols.ID, share.GranteeUser, e.ids["bob"], access.Read); err != nil {
		t.Fatal(err)
	}
	r = e.changesOf(t, "bob", cur, 0)
	if c, _ := decodeCursor(r.Cursor); !r.RootsChanged || len(r.Changes) != 0 || c.S[e.ids["carol"]] == 0 {
		t.Errorf("new share: %+v", r)
	}
	e.file(t, "carol", carols.ID, "x.txt", "x")
	if r := e.changesOf(t, "bob", r.Cursor, 0); len(r.Changes) != 1 || r.Changes[0].Owner != e.ids["carol"] {
		t.Errorf("in the new share: %+v", r)
	}
}

// mirror is a sync client's copy of what it sees, kept from trees and
// changes the way a client would: what a node is, where it is.
type mirror struct {
	t     *testing.T
	e     env
	user  string
	nodes map[string]nodeJSON
	// cursor is where it is in the journals.
	cursor string
}

func newMirror(t *testing.T, e env, user string, roots ...string) *mirror {
	m := &mirror{t: t, e: e, user: user, nodes: map[string]nodeJSON{}}
	m.cursor = e.rootsOf(t, user).Cursor
	for _, r := range roots {
		m.read(r)
	}
	return m
}

func (m *mirror) read(root string) {
	_, nodes := m.e.treeOf(m.t, m.user, root)
	m.drop(nodes[0].ID)
	for _, n := range nodes {
		m.nodes[n.ID] = n
	}
}

// drop removes id and everything below it.
func (m *mirror) drop(id string) {
	below := map[string]bool{id: true}
	for changed := true; changed; {
		changed = false
		for nid, n := range m.nodes {
			if !below[nid] && n.ParentID != nil && below[*n.ParentID] {
				below[nid], changed = true, true
			}
		}
	}
	for nid := range below {
		delete(m.nodes, nid)
	}
}

// sync follows the changes, a few rows at a time, to the end.
func (m *mirror) sync() {
	for {
		r := m.e.changesOf(m.t, m.user, m.cursor, 3)
		for _, c := range r.Changes {
			switch {
			case c.Deleted:
				m.drop(c.ID)
			case c.Subtree:
				m.read(c.ID)
			default:
				m.nodes[c.ID] = *c.Node
			}
		}
		m.cursor = r.Cursor
		if !r.More {
			return
		}
	}
}

// state is what must match: a folder's size, ETag and mtime follow from
// what is below it and aren't journaled.
func state(nodes map[string]nodeJSON) map[string]string {
	out := map[string]string{}
	for id, n := range nodes {
		parent := ""
		if n.ParentID != nil {
			parent = *n.ParentID
		}
		s := fmt.Sprintf("%s/%s %s", parent, n.Name, n.Kind)
		if n.Kind == fs.KindFile {
			s += fmt.Sprintf(" %s %s %d %v", n.ETag, n.SHA256, n.Size, n.Permissions)
		}
		out[id] = s
	}
	return out
}

func (m *mirror) check(step int, roots ...string) {
	m.t.Helper()
	want := map[string]nodeJSON{}
	for _, r := range roots {
		_, nodes := m.e.treeOf(m.t, m.user, r)
		for _, n := range nodes {
			want[n.ID] = n
		}
	}
	got, exp := state(m.nodes), state(want)
	for id, s := range exp {
		if got[id] != s {
			m.t.Errorf("step %d, %s: %s has %s %q, server %q", step, m.user, m.user, id, got[id], s)
		}
	}
	for id, s := range got {
		if _, ok := exp[id]; !ok {
			m.t.Errorf("step %d, %s: has %s %q, gone on the server", step, m.user, id, s)
		}
	}
}

// A client that reads the trees once and then only follows changes has,
// after any operations, exactly what the server has: Alice of her tree,
// Bob of what she shares with him.
func TestMirror(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.ids["alice"]
	root := e.roots["alice"]
	shared := e.mkdir(t, "alice", root.ID, "Shared")
	e.mkdir(t, "alice", root.ID, "Private")
	e.grant(t, shared.ID, "bob", access.Read)
	ma := newMirror(t, e, "alice", root.ID)
	mb := newMirror(t, e, "bob", shared.ID)

	rnd := rand.New(rand.NewPCG(7, 3))
	pick := func(ids []string) string { return ids[rnd.IntN(len(ids))] }
	query := func(q string) []string {
		rows, err := e.pool.Query(ctx, q, alice)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	dirs := func() []string {
		return query(`SELECT id::text FROM nodes WHERE owner_id = $1 AND kind = 'dir' AND trashed_at IS NULL ORDER BY created_at, id`)
	}
	files := func() []string {
		return query(`SELECT id::text FROM nodes WHERE owner_id = $1 AND kind = 'file' AND trashed_at IS NULL ORDER BY created_at, id`)
	}
	// The shared folder itself stays: trashing or moving it is tested
	// above, here it would end Bob's part.
	movable := func() []string {
		return slices.DeleteFunc(query(`SELECT id::text FROM nodes WHERE owner_id = $1 AND parent_id IS NOT NULL AND trashed_at IS NULL ORDER BY created_at, id`),
			func(id string) bool { return id == shared.ID })
	}
	trash := func() []string {
		return query(`SELECT id::text FROM nodes WHERE owner_id = $1 AND trash_root ORDER BY trashed_at, id`)
	}
	name := func() string { return fmt.Sprintf("n%d", rnd.IntN(10)) }
	content := func() string { return fmt.Sprintf("c%d", rnd.IntN(1000)) }
	ops := []func() error{
		func() error { _, err := e.files.CreateDir(ctx, alice, pick(dirs()), name()); return err },
		func() error {
			c := content()
			_, err := e.files.CreateFile(ctx, alice, pick(dirs()), name(), strings.NewReader(c), int64(len(c)))
			return err
		},
		func() error {
			c := content()
			_, _, err := e.files.PutFile(ctx, alice, pick(dirs()), name(), strings.NewReader(c), int64(len(c)), fs.PutCond{})
			return err
		},
		func() error {
			if f := files(); len(f) > 0 {
				c := content()
				_, err := e.files.WriteFile(ctx, alice, pick(f), strings.NewReader(c), int64(len(c)), fs.PutCond{})
				return err
			}
			return nil
		},
		func() error {
			if n := movable(); len(n) > 0 {
				_, err := e.files.Move(ctx, alice, pick(n), pick(dirs()), "")
				return err
			}
			return nil
		},
		func() error {
			if n := movable(); len(n) > 0 {
				_, err := e.files.Rename(ctx, alice, pick(n), name())
				return err
			}
			return nil
		},
		func() error {
			if n := movable(); len(n) > 0 {
				return e.files.Trash(ctx, alice, pick(n))
			}
			return nil
		},
		func() error {
			if n := trash(); len(n) > 0 {
				_, err := e.files.Restore(ctx, alice, pick(n))
				return err
			}
			return nil
		},
		func() error {
			if n := trash(); len(n) > 0 {
				return e.files.Purge(ctx, alice, pick(n))
			}
			return nil
		},
		func() error {
			if n := movable(); len(n) > 0 {
				_, err := e.files.Copy(ctx, alice, pick(n), pick(dirs()), "", fs.CopyOptions{Rename: true})
				return err
			}
			return nil
		},
	}
	weights := []int{4, 4, 3, 2, 4, 2, 2, 2, 1, 1}
	total := 0
	for _, w := range weights {
		total += w
	}
	expected := []error{fs.ErrNameConflict, fs.ErrCycle, fs.ErrNotFound, fs.ErrIsRoot}
	for step := range 300 {
		r := rnd.IntN(total)
		op := 0
		for ; r >= weights[op]; op++ {
			r -= weights[op]
		}
		if err := ops[op](); err != nil && !slices.ContainsFunc(expected, func(want error) bool { return errors.Is(err, want) }) {
			t.Fatalf("step %d, op %d: %v", step, op, err)
		}
		if step%15 == 14 {
			ma.sync()
			mb.sync()
			ma.check(step, root.ID)
			mb.check(step, shared.ID)
		}
	}
	if len(ma.nodes) < 40 || len(mb.nodes) < 10 {
		t.Fatalf("too little to test: alice %d, bob %d nodes", len(ma.nodes), len(mb.nodes))
	}
}

// Every change to what a user sees of others' trees moves their
// access_seq, in its transaction (E11-5): that wakes their long poll and
// says rootsChanged. Changes that touch no one's access leave it.
func TestAccessSeq(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice, bob, carol := e.ids["alice"], e.ids["bob"], e.ids["carol"]
	seq := func(user string) int64 {
		t.Helper()
		v, err := e.files.SyncView(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		return v.Own.AccessSeq
	}
	step := func(name string, moves map[string]int64, do func() error) {
		t.Helper()
		before := map[string]int64{alice: seq(alice), bob: seq(bob), carol: seq(carol)}
		if err := do(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, u := range []string{alice, bob, carol} {
			if got := seq(u) - before[u]; got != moves[u] {
				t.Errorf("%s: access_seq of %s moved by %d, want %d", name, u, got, moves[u])
			}
		}
	}
	root := e.roots["alice"]
	dir := e.mkdir(t, "alice", root.ID, "D")
	var sh share.Share
	step("grant", map[string]int64{bob: 1}, func() (err error) {
		sh, err = e.shares.Grant(ctx, alice, dir.ID, share.GranteeUser, bob, access.Read)
		return err
	})
	step("grant again, other rights", map[string]int64{bob: 1}, func() error {
		_, err := e.shares.Grant(ctx, alice, dir.ID, share.GranteeUser, bob, access.Read|access.Write)
		return err
	})
	step("update", map[string]int64{bob: 1}, func() error {
		_, err := e.shares.Update(ctx, alice, sh.ID, access.Read)
		return err
	})
	step("a write in the tree", nil, func() error {
		_, err := e.files.CreateDir(ctx, alice, dir.ID, "sub")
		return err
	})
	step("revoke", map[string]int64{bob: 1}, func() error { return e.shares.Revoke(ctx, alice, sh.ID) })

	g, err := e.shares.CreateGroup(ctx, "G")
	if err != nil {
		t.Fatal(err)
	}
	step("join", map[string]int64{carol: 1}, func() error { return e.shares.SetMember(ctx, g.ID, carol, true) })
	step("join again", nil, func() error { return e.shares.SetMember(ctx, g.ID, carol, true) })
	step("join, the owner", map[string]int64{alice: 1}, func() error { return e.shares.SetMember(ctx, g.ID, alice, true) })
	step("grant to the group", map[string]int64{alice: 1, carol: 1}, func() error {
		_, err := e.shares.Grant(ctx, alice, dir.ID, share.GranteeGroup, g.ID, access.Read)
		return err
	})
	step("leave", map[string]int64{alice: 1}, func() error { return e.shares.SetMember(ctx, g.ID, alice, false) })
	step("delete the group", map[string]int64{carol: 1}, func() error { return e.shares.DeleteGroup(ctx, g.ID) })

	// A purge takes the shares below with it.
	inner := e.mkdir(t, "alice", dir.ID, "inner")
	e.grant(t, inner.ID, "bob", access.Read)
	e.grant(t, dir.ID, "carol", access.Read)
	step("trash", nil, func() error { return e.files.Trash(ctx, alice, dir.ID) })
	step("purge", map[string]int64{bob: 1, carol: 1}, func() error { return e.files.Purge(ctx, alice, dir.ID) })

	// A cursor whose access_seq is behind says rootsChanged, the shares
	// alike or not.
	cur, err := decodeCursor(e.rootsOf(t, "bob").Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if r := e.changesOf(t, "bob", cur.encode(), 0); r.RootsChanged {
		t.Fatalf("roots changed at the head: %+v", r)
	}
	cur.A--
	if r := e.changesOf(t, "bob", cur.encode(), 0); !r.RootsChanged {
		t.Errorf("stale access_seq: %+v", r)
	}
}

// The owner's account deleted (E9-11's DeleteOwner): those they shared
// with are woken and told rootsChanged — as for a revoked share, the
// shared root is no longer among their roots, and the client drops what
// it had of it — with a cursor that no longer follows that tree; no 410.
func TestOwnerDeleted(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.roots["alice"]
	dir := e.mkdir(t, "alice", root.ID, "D")
	f := e.file(t, "alice", dir.ID, "f.txt", "f")
	e.grant(t, dir.ID, "bob", access.Read)
	bob := e.rootsOf(t, "bob")
	got := e.poll(t, "bob", bob.Cursor, 30)
	e.waitFor(t, 1)
	if err := e.files.DeleteOwner(ctx, e.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	// Woken by the trash (a tombstone) or the purge's access_seq,
	// whichever the hub saw first.
	if p := <-got; p.code != http.StatusOK || len(p.resp.Changes) == 0 && !p.resp.RootsChanged {
		t.Fatalf("owner deleted: %d %+v", p.code, p.resp)
	}
	r := e.changesOf(t, "bob", bob.Cursor, 0)
	if !r.RootsChanged || len(r.Changes) != 0 {
		t.Errorf("owner deleted: %+v", r)
	}
	if roots := e.rootsOf(t, "bob"); len(roots.Roots) != 1 {
		t.Errorf("roots: %+v", roots)
	}
	if c, _ := decodeCursor(r.Cursor); len(c.S) != 0 {
		t.Errorf("cursor still follows the owner: %+v", c)
	}
	// The account row goes last, with its journal.
	if _, err := e.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, e.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if r := e.changesOf(t, "bob", bob.Cursor, 0); !r.RootsChanged || len(r.Changes) != 0 {
		t.Errorf("owner gone: %+v", r)
	}
	if rec := e.do("bob", "/api/v1/sync/tree?root="+f.ID); rec.Code != http.StatusNotFound {
		t.Errorf("tree of the gone owner's file: %d", rec.Code)
	}
}

// Retention (E11-6): what PruneJournal deleted, a cursor can't resume
// from; a cursor past it carries on.
func TestJournalRetention(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.roots["alice"]
	old := e.rootsOf(t, "alice").Cursor
	e.file(t, "alice", root.ID, "a.txt", "a")
	e.file(t, "alice", root.ID, "b.txt", "b")
	mid := e.rootsOf(t, "alice").Cursor
	c := e.file(t, "alice", root.ID, "c.txt", "c")
	if _, err := e.pool.Exec(ctx, `UPDATE sync_journal SET at = now() - interval '31 days' WHERE owner_id = $1 AND node_id <> $2`, e.ids["alice"], c.ID); err != nil {
		t.Fatal(err)
	}
	n, err := e.files.PruneJournal(ctx, time.Now().Add(-30*24*time.Hour), 1000)
	if err != nil || n < 2 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if rec := e.do("alice", "/api/v1/sync/changes?cursor="+url.QueryEscape(old)); rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), `"cursor_expired"`) {
		t.Errorf("old cursor: %d %s", rec.Code, rec.Body)
	}
	if r := e.changesOf(t, "alice", mid, 0); !slices.Equal(ids(r.Changes), []string{c.ID}) {
		t.Errorf("cursor past the pruned rows: %+v", r)
	}
	// Nothing older left: a second run deletes nothing.
	if n, err := e.files.PruneJournal(ctx, time.Now().Add(-30*24*time.Hour), 1000); err != nil || n != 0 {
		t.Errorf("again: %d %v", n, err)
	}
}
