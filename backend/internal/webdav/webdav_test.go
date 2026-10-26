package webdav

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

// queryCounter counts the statements the pool runs.
type queryCounter struct{ n atomic.Int64 }

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type env struct {
	h       http.Handler
	svc     *fs.Service
	shares  *share.Service
	pool    *pgxpool.Pool
	queries *queryCounter
	// users by name: id and app password.
	ids, passwords map[string]string
}

func newEnv(t *testing.T) env {
	t.Helper()
	url := testDatabaseURL()
	if err := db.Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	queries := &queryCounter{}
	cfg.ConnConfig.Tracer = queries
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE users, nodes, blobs, versions, groups CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e := env{svc: fs.New(pool, store), pool: pool, queries: queries, ids: map[string]string{}, passwords: map[string]string{}}
	apps := auth.NewAppPasswords(pool)
	for _, name := range []string{"alice", "bob", "carol"} {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO users (username) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.EnsureRoot(ctx, id); err != nil {
			t.Fatal(err)
		}
		_, password, err := apps.Create(ctx, id, "test")
		if err != nil {
			t.Fatal(err)
		}
		e.ids[name], e.passwords[name] = id, password
	}
	e.shares = share.New(pool, e.svc)
	e.h = auth.BasicAuth(apps, nil)(New(e.svc, e.shares, nil)("/dav/files"))
	return e
}

// do sends a request as alice to her tree; path is below it.
func (e env) do(t *testing.T, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	return e.as(t, "alice", method, "/dav/files/alice"+path, body, hdr...)
}

// as sends a request as user to a full path; hdr are name, value pairs.
func (e env) as(t *testing.T, user, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	// Raw paths in tests; on the wire they are escaped.
	req := httptest.NewRequest(method, escaped(path), strings.NewReader(body))
	if user != "" {
		req.SetBasicAuth(user, e.passwords[user])
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func escaped(path string) string { return (&url.URL{Path: path}).EscapedPath() }

func status(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body %q", rec.Code, want, rec.Body.String())
	}
}

type multistatus struct {
	Responses []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Prop struct {
				Inner []struct {
					XMLName  xml.Name
					InnerXML string `xml:",innerxml"`
				} `xml:",any"`
			} `xml:"prop"`
			Status string `xml:"status"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// found returns href -> property local name -> value (inner XML) of the
// properties answered with 200.
func found(t *testing.T, rec *httptest.ResponseRecorder) map[string]map[string]string {
	t.Helper()
	status(t, rec, statusMulti)
	var ms multistatus
	if err := xml.Unmarshal(rec.Body.Bytes(), &ms); err != nil {
		t.Fatalf("multistatus: %v\n%s", err, rec.Body.String())
	}
	out := map[string]map[string]string{}
	for _, r := range ms.Responses {
		props := map[string]string{}
		for _, ps := range r.Propstat {
			if !strings.Contains(ps.Status, " 200 ") {
				continue
			}
			for _, p := range ps.Prop.Inner {
				props[p.XMLName.Local] = p.InnerXML
			}
		}
		out[r.Href] = props
	}
	return out
}

const propsBody = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop>
	<d:resourcetype/><d:getetag/><d:getcontentlength/><d:getcontenttype/><d:getlastmodified/>
	<d:quota-used-bytes/><d:quota-available-bytes/><oc:fileid/></d:prop></d:propfind>`

func TestOnlyOwnTree(t *testing.T) {
	e := newEnv(t)
	status(t, e.as(t, "", "PROPFIND", "/dav/files/alice/", "", "Depth", "0"), http.StatusUnauthorized)
	status(t, e.as(t, "bob", "PROPFIND", "/dav/files/alice/", "", "Depth", "0"), http.StatusNotFound)
	status(t, e.as(t, "bob", "PUT", "/dav/files/alice/x.txt", "x"), http.StatusNotFound)
	// Moving into another user's tree isn't a destination this server
	// writes to.
	status(t, e.do(t, "PUT", "/a.txt", "a"), http.StatusCreated)
	status(t, e.do(t, "MOVE", "/a.txt", "", "Destination", "/dav/files/bob/a.txt"), http.StatusBadGateway)
	status(t, e.as(t, "alice", "PROPFIND", "/dav/files/ALICE/", "", "Depth", "0"), statusMulti)
}

func TestFilesAndFolders(t *testing.T) {
	e := newEnv(t)
	status(t, e.do(t, "MKCOL", "/Docs", ""), http.StatusCreated)
	status(t, e.do(t, "MKCOL", "/Docs", ""), http.StatusMethodNotAllowed)
	status(t, e.do(t, "MKCOL", "/Нет/Такой", ""), http.StatusConflict)
	status(t, e.do(t, "PUT", "/Нет/a.txt", "a"), http.StatusConflict)

	put := e.do(t, "PUT", "/Docs/отчёт.txt", "первая")
	status(t, put, http.StatusCreated)
	first := put.Header().Get("ETag")
	if first == "" {
		t.Fatal("PUT without ETag")
	}
	put = e.do(t, "PUT", "/Docs/отчёт.txt", "вторая версия")
	status(t, put, http.StatusNoContent)
	// A folder is no file to PUT over.
	status(t, e.do(t, "PUT", "/Docs", "x"), http.StatusMethodNotAllowed)

	// Names match without regard to case, as the tree keeps them unique.
	get := e.do(t, "GET", "/docs/ОТЧЁТ.txt", "")
	status(t, get, http.StatusOK)
	if get.Body.String() != "вторая версия" || get.Header().Get("ETag") != put.Header().Get("ETag") {
		t.Fatalf("GET = %q, ETag %q", get.Body.String(), get.Header().Get("ETag"))
	}
	ranged := e.do(t, "GET", "/Docs/отчёт.txt", "", "Range", "bytes=0-9")
	status(t, ranged, http.StatusPartialContent)
	status(t, e.do(t, "GET", "/Docs", ""), http.StatusMethodNotAllowed)
	versions, _, err := e.svc.ListVersions(context.Background(), e.ids["alice"], mustLookup(t, e, "Docs", "отчёт.txt").ID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("versions = %v, %v; the overwritten content should be one", versions, err)
	}

	got := found(t, e.do(t, "PROPFIND", "/Docs/", propsBody, "Depth", "1"))
	dir, file := got["/dav/files/alice/Docs/"], got["/dav/files/alice/Docs/%D0%BE%D1%82%D1%87%D1%91%D1%82.txt"]
	if dir == nil || file == nil {
		t.Fatalf("hrefs: %v", got)
	}
	if dir["resourcetype"] != "<D:collection></D:collection>" && dir["resourcetype"] != "<D:collection/>" || dir["getetag"] == "" || dir["quota-used-bytes"] != "25" {
		t.Errorf("folder props: %v", dir)
	}
	if _, ok := dir["quota-available-bytes"]; ok {
		t.Errorf("no quota, yet available bytes: %v", dir)
	}
	if file["getcontentlength"] != "25" || file["getcontenttype"] != "text/plain; charset=utf-8" ||
		file["getetag"] != put.Header().Get("ETag") || file["fileid"] == "" {
		t.Errorf("file props: %v", file)
	}

	status(t, e.do(t, "PROPFIND", "/Docs/", propsBody, "Depth", "infinity"), http.StatusForbidden)
	status(t, e.do(t, "PROPFIND", "/Docs/", propsBody), http.StatusForbidden)
	status(t, e.do(t, "PROPFIND", "/Нет/", propsBody, "Depth", "0"), http.StatusNotFound)
	// An empty body asks for everything RFC 4918 defines.
	all := found(t, e.do(t, "PROPFIND", "/Docs/отчёт.txt", "", "Depth", "0"))
	for _, name := range []string{"getetag", "getlastmodified", "displayname", "supportedlock"} {
		if _, ok := all["/dav/files/alice/Docs/%D0%BE%D1%82%D1%87%D1%91%D1%82.txt"][name]; !ok {
			t.Errorf("allprop without %s: %v", name, all)
		}
	}
}

func mustLookup(t *testing.T, e env, names ...string) fs.Node {
	t.Helper()
	root, err := e.svc.Root(context.Background(), e.ids["alice"])
	if err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.Lookup(context.Background(), e.ids["alice"], root.ID, names)
	if err != nil {
		t.Fatalf("lookup %v: %v", names, err)
	}
	return n
}

func TestConditionalPut(t *testing.T) {
	e := newEnv(t)
	etag := e.do(t, "PUT", "/a.txt", "1").Header().Get("ETag")
	// A sync client that saw an older version must not overwrite.
	status(t, e.do(t, "PUT", "/a.txt", "2", "If-Match", `"stale"`), http.StatusPreconditionFailed)
	status(t, e.do(t, "PUT", "/a.txt", "2", "If-None-Match", "*"), http.StatusPreconditionFailed)
	status(t, e.do(t, "PUT", "/new.txt", "2", "If-Match", "*"), http.StatusPreconditionFailed)
	status(t, e.do(t, "PUT", "/a.txt", "2", "If-Match", etag), http.StatusNoContent)
	if body := e.do(t, "GET", "/a.txt", "").Body.String(); body != "2" {
		t.Fatalf("content = %q", body)
	}
}

func TestQuota(t *testing.T) {
	e := newEnv(t)
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET quota_bytes = 10 WHERE id = $1`, e.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	status(t, e.do(t, "PUT", "/big.bin", strings.Repeat("x", 11)), statusInsufficientStorage)
	status(t, e.do(t, "GET", "/big.bin", ""), http.StatusNotFound)
	status(t, e.do(t, "PUT", "/a.bin", "1234"), http.StatusCreated)
	root := found(t, e.do(t, "PROPFIND", "/", propsBody, "Depth", "0"))["/dav/files/alice/"]
	if root["quota-available-bytes"] != "6" || root["quota-used-bytes"] != "4" {
		t.Fatalf("quota props: %v", root)
	}
	// A copy counts too.
	status(t, e.do(t, "COPY", "/a.bin", "", "Destination", "/dav/files/alice/b.bin"), http.StatusCreated)
	status(t, e.do(t, "COPY", "/a.bin", "", "Destination", "/dav/files/alice/c.bin"), statusInsufficientStorage)
}

func TestMoveCopyDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	status(t, e.do(t, "MKCOL", "/src", ""), http.StatusCreated)
	status(t, e.do(t, "MKCOL", "/src/sub", ""), http.StatusCreated)
	status(t, e.do(t, "PUT", "/src/sub/a.txt", "a"), http.StatusCreated)
	status(t, e.do(t, "PUT", "/b.txt", "b"), http.StatusCreated)
	status(t, e.do(t, "PUT", "/c.txt", "c"), http.StatusCreated)

	status(t, e.do(t, "COPY", "/src", "", "Destination", "/dav/files/alice/copy"), http.StatusCreated)
	if body := e.do(t, "GET", "/copy/sub/a.txt", "").Body.String(); body != "a" {
		t.Fatalf("copied content = %q", body)
	}
	status(t, e.do(t, "COPY", "/src", "", "Destination", "/dav/files/alice/src/sub/in"), http.StatusConflict)
	status(t, e.do(t, "COPY", "/src", "", "Destination", "/dav/files/alice/empty", "Depth", "0"), http.StatusCreated)
	if got := found(t, e.do(t, "PROPFIND", "/empty/", "", "Depth", "1")); len(got) != 1 {
		t.Fatalf("Depth 0 copy took the contents: %v", got)
	}

	status(t, e.do(t, "MOVE", "/b.txt", "", "Destination", "/dav/files/alice/c.txt", "Overwrite", "F"), http.StatusPreconditionFailed)
	status(t, e.do(t, "MOVE", "/b.txt", "", "Destination", "/dav/files/alice/c.txt"), http.StatusNoContent)
	if body := e.do(t, "GET", "/c.txt", "").Body.String(); body != "b" {
		t.Fatalf("moved content = %q", body)
	}
	status(t, e.do(t, "GET", "/b.txt", ""), http.StatusNotFound)
	// Only the case changes: the same file, renamed.
	status(t, e.do(t, "MOVE", "/c.txt", "", "Destination", "/dav/files/alice/C.TXT"), http.StatusCreated)
	if mustLookup(t, e, "c.txt").Name != "C.TXT" {
		t.Fatal("case-only rename lost")
	}
	// Over the folder holding the source.
	status(t, e.do(t, "MOVE", "/src/sub", "", "Destination", "/dav/files/alice/src"), http.StatusConflict)
	status(t, e.do(t, "MOVE", "/src", "", "Destination", "/dav/files/alice/src/sub/x"), http.StatusConflict)
	status(t, e.do(t, "MOVE", "/src", "", "Destination", "/dav/files/alice/src"), http.StatusForbidden)

	status(t, e.do(t, "DELETE", "/src", ""), http.StatusNoContent)
	status(t, e.do(t, "DELETE", "/src", ""), http.StatusNotFound)
	status(t, e.do(t, "DELETE", "/", ""), http.StatusForbidden)
	trash, _, err := e.svc.ListTrash(ctx, e.ids["alice"], 10, "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, item := range trash {
		names = append(names, item.Name)
	}
	// What was deleted or overwritten can be restored.
	if strings.Join(names, ",") != "src,c.txt" {
		t.Fatalf("trash = %v", names)
	}
}

func TestProppatch(t *testing.T) {
	e := newEnv(t)
	status(t, e.do(t, "PUT", "/a.txt", "a"), http.StatusCreated)
	set := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:schemas-microsoft-com:">
		<D:set><D:prop><Z:Win32LastModifiedTime>Wed, 24 Oct 2026 10:00:00 GMT</Z:Win32LastModifiedTime>
		<Z:nested><Z:x a="1">v</Z:x><plain/></Z:nested></D:prop></D:set></D:propertyupdate>`
	got := found(t, e.do(t, "PROPPATCH", "/a.txt", set))
	if len(got["/dav/files/alice/a.txt"]) != 2 {
		t.Fatalf("proppatch: %v", got)
	}
	ask := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:" xmlns:Z="urn:schemas-microsoft-com:"><D:prop>
		<Z:Win32LastModifiedTime/><Z:nested/><Z:missing/></D:prop></D:propfind>`
	rec := e.do(t, "PROPFIND", "/a.txt", ask, "Depth", "0")
	props := found(t, rec)["/dav/files/alice/a.txt"]
	if props["Win32LastModifiedTime"] != "Wed, 24 Oct 2026 10:00:00 GMT" {
		t.Fatalf("dead prop: %v", props)
	}
	// The value comes back in its own namespaces, wherever written.
	var nested struct {
		X struct {
			A     string `xml:"a,attr"`
			Value string `xml:",chardata"`
		} `xml:"urn:schemas-microsoft-com: x"`
		Plain *struct {
			XMLName xml.Name
		} `xml:"plain"`
	}
	if err := xml.Unmarshal([]byte("<r>"+props["nested"]+"</r>"), &nested); err != nil || nested.X.Value != "v" || nested.X.A != "1" || nested.Plain == nil || nested.Plain.XMLName.Space != "" {
		t.Fatalf("nested value %q: %+v, %v", props["nested"], nested, err)
	}
	if !strings.Contains(rec.Body.String(), "404 Not Found") {
		t.Fatalf("a missing property isn't 404: %s", rec.Body.String())
	}

	// A live property can't be set, and fails the rest of the patch.
	live := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:z"><D:set><D:prop>
		<D:getetag>x</D:getetag><Z:other>y</Z:other></D:prop></D:set></D:propertyupdate>`
	rec = e.do(t, "PROPPATCH", "/a.txt", live)
	status(t, rec, statusMulti)
	if !strings.Contains(rec.Body.String(), "403 Forbidden") || !strings.Contains(rec.Body.String(), "424 Failed Dependency") {
		t.Fatalf("protected property: %s", rec.Body.String())
	}
	remove := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:schemas-microsoft-com:">
		<D:remove><D:prop><Z:nested/></D:prop></D:remove></D:propertyupdate>`
	status(t, e.do(t, "PROPPATCH", "/a.txt", remove), statusMulti)
	if _, ok := found(t, e.do(t, "PROPFIND", "/a.txt", ask, "Depth", "0"))["/dav/files/alice/a.txt"]["nested"]; ok {
		t.Fatal("removed property still there")
	}
	// In document order: removed, then set again, it stays.
	removeSet := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:schemas-microsoft-com:">
		<D:remove><D:prop><Z:nested/></D:prop></D:remove><D:set><D:prop><Z:nested>again</Z:nested></D:prop></D:set></D:propertyupdate>`
	status(t, e.do(t, "PROPPATCH", "/a.txt", removeSet), statusMulti)
	if v := found(t, e.do(t, "PROPFIND", "/a.txt", ask, "Depth", "0"))["/dav/files/alice/a.txt"]["nested"]; v != "again" {
		t.Fatalf("remove then set: %q", v)
	}
	// Dead properties move with the node.
	status(t, e.do(t, "MOVE", "/a.txt", "", "Destination", "/dav/files/alice/b.txt"), http.StatusCreated)
	if _, ok := found(t, e.do(t, "PROPFIND", "/b.txt", ask, "Depth", "0"))["/dav/files/alice/b.txt"]["Win32LastModifiedTime"]; !ok {
		t.Fatal("property lost on move")
	}
}

func TestLocks(t *testing.T) {
	e := newEnv(t)
	lockBody := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope>
		<D:locktype><D:write/></D:locktype><D:owner>alice</D:owner></D:lockinfo>`
	// Locking a free name reserves it with an empty file.
	rec := e.do(t, "LOCK", "/doc.txt", lockBody, "Timeout", "Second-60")
	status(t, rec, http.StatusCreated)
	token := rec.Header().Get("Lock-Token")
	if token == "" || !strings.Contains(rec.Body.String(), "Second-60") {
		t.Fatalf("lock: %q %s", token, rec.Body.String())
	}
	status(t, e.do(t, "PUT", "/doc.txt", "x"), statusLocked)
	status(t, e.do(t, "DELETE", "/doc.txt", ""), statusLocked)
	status(t, e.do(t, "PROPPATCH", "/doc.txt", `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:"><D:set><D:prop><x xmlns="urn:x">1</x></D:prop></D:set></D:propertyupdate>`), statusLocked)
	// Through the alias, with other case: the same lock.
	status(t, e.do(t, "PUT", "/DOC.txt", "x"), statusLocked)
	status(t, e.do(t, "PUT", "/doc.txt", "x", "If", "("+token+")"), http.StatusNoContent)
	// The ETag in a condition must hold too.
	status(t, e.do(t, "PUT", "/doc.txt", "x", "If", "("+token+` ["bogus"])`), http.StatusPreconditionFailed)
	etag := e.do(t, "GET", "/doc.txt", "").Header().Get("ETag")
	status(t, e.do(t, "PUT", "/doc.txt", "z", "If", "("+token+" ["+etag+"])"), http.StatusNoContent)
	status(t, e.do(t, "LOCK", "/doc.txt", lockBody), statusLocked)
	// The lock shows in PROPFIND, and the web UI's way in respects it.
	disc := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop><D:lockdiscovery/></D:prop></D:propfind>`
	if body := e.do(t, "PROPFIND", "/doc.txt", disc, "Depth", "0").Body.String(); !strings.Contains(body, strings.Trim(token, "<>")) ||
		!strings.Contains(body, "<D:lockroot><D:href>/dav/files/alice/doc.txt</D:href>") {
		t.Fatalf("lockdiscovery: %s", body)
	}
	if _, err := e.svc.Rename(context.Background(), e.ids["alice"], mustLookup(t, e, "doc.txt").ID, "x.txt"); err == nil {
		t.Fatal("renamed a locked file outside WebDAV")
	}
	// "No lock" holds of any resource: the usual way to send ETag-only or
	// token-free conditions.
	status(t, e.do(t, "PUT", "/doc.txt", "x", "If", "(Not <DAV:no-lock>)"), statusLocked)
	status(t, e.do(t, "UNLOCK", "/doc.txt", "", "Lock-Token", "<opaquelocktoken:nope>"), http.StatusConflict)
	status(t, e.do(t, "UNLOCK", "/doc.txt", "", "Lock-Token", token), http.StatusNoContent)
	status(t, e.do(t, "PUT", "/doc.txt", "y"), http.StatusNoContent)

	// A deep lock on a folder: nothing in it changes without the token,
	// sent tagged with the folder it belongs to.
	status(t, e.do(t, "MKCOL", "/dir", ""), http.StatusCreated)
	status(t, e.do(t, "PUT", "/dir/a.txt", "a"), http.StatusCreated)
	rec = e.do(t, "LOCK", "/dir", lockBody)
	status(t, rec, http.StatusOK)
	dirToken := rec.Header().Get("Lock-Token")
	status(t, e.do(t, "PUT", "/dir/a.txt", "b"), statusLocked)
	status(t, e.do(t, "PUT", "/dir/new.txt", "b"), statusLocked)
	status(t, e.do(t, "LOCK", "/dir/a.txt", lockBody), statusLocked)
	status(t, e.do(t, "PUT", "/dir/a.txt", "b", "If", "</dav/files/alice/dir> ("+dirToken+")"), http.StatusNoContent)
	status(t, e.do(t, "PUT", "/dir/a.txt", "c", "If", "("+dirToken+")"), http.StatusNoContent)
	got := found(t, e.do(t, "PROPFIND", "/dir/", disc, "Depth", "1"))
	if !strings.Contains(got["/dav/files/alice/dir/a.txt"]["lockdiscovery"], "<D:lockroot><D:href>/dav/files/alice/dir/</D:href>") {
		t.Fatalf("inherited lockdiscovery: %v", got)
	}
	// Refreshed by an empty LOCK naming the token.
	status(t, e.do(t, "LOCK", "/dir", "", "If", "("+dirToken+")", "Timeout", "Second-120"), http.StatusOK)
}

// PROPFIND Depth 1 builds its answer from one listing: the statements it
// runs don't grow with the number of children.
func TestPropfindQueriesDontGrowWithChildren(t *testing.T) {
	e := newEnv(t)
	status(t, e.do(t, "MKCOL", "/few", ""), http.StatusCreated)
	status(t, e.do(t, "MKCOL", "/many", ""), http.StatusCreated)
	for i := range 50 {
		status(t, e.do(t, "PUT", fmt.Sprintf("/many/%d.txt", i), "x"), http.StatusCreated)
		if i < 2 {
			status(t, e.do(t, "PUT", fmt.Sprintf("/few/%d.txt", i), "x"), http.StatusCreated)
		}
	}
	count := func(path string) int64 {
		before := e.queries.n.Load()
		rec := e.do(t, "PROPFIND", path, "", "Depth", "1")
		status(t, rec, statusMulti)
		_, _ = io.Copy(io.Discard, rec.Body)
		return e.queries.n.Load() - before
	}
	few, many := count("/few/"), count("/many/")
	if few != many {
		t.Fatalf("PROPFIND ran %d statements for 2 children, %d for 50", few, many)
	}
}
