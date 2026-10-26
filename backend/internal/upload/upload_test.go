package upload

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/db"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/storage"
	"github.com/do-service/cloud/backend/internal/storage/localfs"
)

func testDatabaseURL() string {
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
}

type env struct {
	srv     *httptest.Server
	pool    *pgxpool.Pool
	files   *fs.Service
	dataDir string
	users   map[string]string // username → id
	cookies map[string]string // username → session cookie
	root    fs.Node
	// rereads counts reads of tus data by fs: finishing with the running
	// hash makes none.
	rereads *atomic.Int64
}

// countingStore counts reads of tus data.
type countingStore struct {
	storage.Backend
	rereads *atomic.Int64
}

func (s countingStore) Get(ctx context.Context, key string, rng *storage.Range) (io.ReadCloser, error) {
	if strings.HasPrefix(key, stagingArea+"/") {
		s.rereads.Add(1)
	}
	return s.Backend.Get(ctx, key, rng)
}

func newEnv(t *testing.T) env {
	t.Helper()
	url := testDatabaseURL()
	if err := db.Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, url, 8, 0, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE users, nodes, blobs, versions, uploads CASCADE`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rereads := new(atomic.Int64)
	e := env{pool: pool, files: fs.New(pool, countingStore{store, rereads}), dataDir: dir, users: map[string]string{}, cookies: map[string]string{}, rereads: rereads}
	sessions := auth.NewSessions(pool, time.Hour)
	for _, name := range []string{"alice", "bob"} {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO users (username) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		root, err := e.files.EnsureRoot(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if name == "alice" {
			e.root = root
		}
		e.users[name] = id
		if e.cookies[name], _, err = sessions.Create(ctx, id, netip.Addr{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	withUser := auth.Middleware(sessions, nil, auth.Cookies{})
	h, err := New(e.files, dir, func(next http.Handler) http.Handler { return withUser(auth.RequireUser(next)) })
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

// do sends one tus request as user ("" for nobody).
func (e env) do(t *testing.T, user, method, url string, header map[string]string, body string) *http.Response {
	t.Helper()
	if strings.HasPrefix(url, "/") {
		url = e.srv.URL + url
	}
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Tus-Resumable", "1.0.0")
	if user != "" {
		req.AddCookie(&http.Cookie{Name: "session", Value: e.cookies[user]})
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func metadata(kv ...string) string {
	var parts []string
	for i := 0; i < len(kv); i += 2 {
		parts = append(parts, kv[i]+" "+base64.StdEncoding.EncodeToString([]byte(kv[i+1])))
	}
	return strings.Join(parts, ",")
}

// create starts an upload as alice and returns its URL, failing the test
// unless the server answers want.
func (e env) create(t *testing.T, size int, want int, meta ...string) (*http.Response, string) {
	t.Helper()
	resp := e.do(t, "alice", "POST", BasePath, map[string]string{"Upload-Length": strconv.Itoa(size), "Upload-Metadata": metadata(meta...)}, "")
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: %d %s, want %d", resp.StatusCode, body, want)
	}
	return resp, resp.Header.Get("Location")
}

func (e env) patch(t *testing.T, user, url string, offset int, chunk string) *http.Response {
	t.Helper()
	return e.do(t, user, "PATCH", url, map[string]string{"Content-Type": "application/offset+octet-stream", "Upload-Offset": strconv.Itoa(offset)}, chunk)
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct{ Code string }
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("not an API error: %d %q", resp.StatusCode, raw)
	}
	return body.Code
}

func (e env) reserved(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM uploads`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUploadInChunks(t *testing.T) {
	e := newEnv(t)
	content := strings.Repeat("0123456789", 1000)
	sum := sha256.Sum256([]byte(content))
	_, url := e.create(t, len(content), http.StatusCreated, "filename", "отчёт.txt", "parentId", "root", "sha256", hex.EncodeToString(sum[:]))
	// A path, not http://host/…: behind a proxy that ends TLS the host
	// seen here is not the one the browser is on.
	if !strings.HasPrefix(url, BasePath) {
		t.Fatalf("Location %q", url)
	}

	if resp := e.patch(t, "alice", url, 0, content[:4000]); resp.StatusCode != http.StatusNoContent || resp.Header.Get("Upload-Offset") != "4000" {
		t.Fatalf("first chunk: %d %v", resp.StatusCode, resp.Header)
	}
	// After a broken connection the client asks where to resume.
	if resp := e.do(t, "alice", "HEAD", url, nil, ""); resp.Header.Get("Upload-Offset") != "4000" || resp.Header.Get("Upload-Length") != "10000" {
		t.Fatalf("head: %d %v", resp.StatusCode, resp.Header)
	}
	if resp := e.patch(t, "alice", url, 3000, content[3000:]); resp.StatusCode != http.StatusConflict {
		t.Fatalf("wrong offset: %d", resp.StatusCode)
	}
	resp := e.patch(t, "alice", url, 4000, content[4000:])
	nodeID := resp.Header.Get(NodeIDHeader)
	if resp.StatusCode != http.StatusNoContent || nodeID == "" {
		t.Fatalf("last chunk: %d %v", resp.StatusCode, resp.Header)
	}

	n, err := e.files.Get(context.Background(), e.users["alice"], nodeID)
	if err != nil || n.Name != "отчёт.txt" || n.Size != int64(len(content)) || n.ParentID != e.root.ID || string(n.ContentHash) != string(sum[:]) {
		t.Fatalf("node: %+v, %v", n, err)
	}
	if got, err := os.ReadFile(filepath.Join(e.dataDir, "files", e.users["alice"], "отчёт.txt")); err != nil || string(got) != content {
		t.Fatalf("on disk: %d bytes, %v", len(got), err)
	}
	if left, _ := os.ReadDir(filepath.Join(e.dataDir, "uploads", "tus")); len(left) != 0 || e.reserved(t) != 0 {
		t.Fatalf("leftovers: %d files, %d reservations", len(left), e.reserved(t))
	}
}

func TestUploadRejects(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.files.CreateFile(ctx, e.users["alice"], e.root.ID, "taken.txt", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE users SET quota_bytes = 100 WHERE username = 'alice'`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		size   int
		meta   []string
		status int
		code   string
	}{
		{"over quota", 100, []string{"filename", "big"}, 413, "quota_exceeded"},
		{"name taken", 1, []string{"filename", "TAKEN.txt", "onConflict", "fail"}, 409, "name_conflict"},
		{"bad name", 1, []string{"filename", "a/b"}, 400, "invalid_name"},
		{"no name", 1, nil, 400, "invalid_name"},
		{"missing folder", 1, []string{"filename", "x", "parentId", "00000000-0000-0000-0000-000000000000"}, 404, "not_found"},
		{"bad checksum", 1, []string{"filename", "x", "sha256", "zz"}, 400, "invalid_request"},
		{"bad policy", 1, []string{"filename", "x", "onConflict", "overwrite"}, 400, "invalid_request"},
	} {
		resp, _ := e.create(t, tc.size, tc.status, tc.meta...)
		if got := errorCode(t, resp); got != tc.code {
			t.Errorf("%s: code %q, want %q", tc.name, got, tc.code)
		}
	}
	resp := e.do(t, "alice", "POST", BasePath, map[string]string{"Upload-Defer-Length": "1", "Upload-Metadata": metadata("filename", "x")}, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("deferred length: %d", resp.StatusCode)
	}
	if resp := e.do(t, "", "POST", BasePath, map[string]string{"Upload-Length": "1"}, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("signed out: %d", resp.StatusCode)
	}
	if n := e.reserved(t); n != 0 {
		t.Errorf("%d reservations left by refused uploads", n)
	}
}

// A wrong checksum is found at the end: the upload is discarded with its
// reservation, and nothing appears in the tree.
func TestUploadChecksumMismatch(t *testing.T) {
	e := newEnv(t)
	sum := sha256.Sum256([]byte("other"))
	_, url := e.create(t, 5, http.StatusCreated, "filename", "a.txt", "sha256", hex.EncodeToString(sum[:]))
	resp := e.patch(t, "alice", url, 0, "hello")
	if resp.StatusCode != http.StatusConflict || errorCode(t, resp) != "checksum_mismatch" {
		t.Fatalf("finish: %d", resp.StatusCode)
	}
	if resp := e.do(t, "alice", "HEAD", url, nil, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("discarded upload still there: %d", resp.StatusCode)
	}
	items, _, err := e.files.List(context.Background(), e.users["alice"], e.root.ID, fs.ListOptions{})
	if err != nil || len(items) != 0 || e.reserved(t) != 0 {
		t.Fatalf("tree %v, %v, reservations %d", items, err, e.reserved(t))
	}
}

func TestUploadOwnership(t *testing.T) {
	e := newEnv(t)
	_, url := e.create(t, 5, http.StatusCreated, "filename", "a.txt")
	for _, method := range []string{"HEAD", "PATCH", "DELETE"} {
		resp := e.do(t, "bob", method, url, map[string]string{"Content-Type": "application/offset+octet-stream", "Upload-Offset": "0"}, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("bob %s: %d", method, resp.StatusCode)
		}
	}
	// The owner may cancel, which ends the reservation too.
	if resp := e.do(t, "alice", "DELETE", url, nil, ""); resp.StatusCode != http.StatusNoContent || e.reserved(t) != 0 {
		t.Fatalf("cancel: %d, reservations %d", resp.StatusCode, e.reserved(t))
	}
}

func TestUploadRenameOnConflict(t *testing.T) {
	e := newEnv(t)
	if _, err := e.files.CreateFile(context.Background(), e.users["alice"], e.root.ID, "a.txt", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	// rename is the default policy.
	_, url := e.create(t, 2, http.StatusCreated, "filename", "a.txt")
	resp := e.patch(t, "alice", url, 0, "hi")
	n, err := e.files.Get(context.Background(), e.users["alice"], resp.Header.Get(NodeIDHeader))
	if err != nil || n.Name != "a (2).txt" {
		t.Fatalf("renamed: %+v, %v", n, err)
	}
}

// replace makes the upload a new version of the file with the name.
func TestUploadReplace(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	old, err := e.files.CreateFile(ctx, e.users["alice"], e.root.ID, "a.txt", strings.NewReader("old"), 3)
	if err != nil {
		t.Fatal(err)
	}
	_, url := e.create(t, 3, http.StatusCreated, "filename", "a.txt", "onConflict", "replace")
	resp := e.patch(t, "alice", url, 0, "new")
	if resp.Header.Get(NodeIDHeader) != old.ID {
		t.Fatalf("replaced into %q, want %s", resp.Header.Get(NodeIDHeader), old.ID)
	}
	versions, _, err := e.files.ListVersions(ctx, e.users["alice"], old.ID)
	if err != nil || len(versions) != 1 || versions[0].Size != 3 {
		t.Fatalf("versions: %+v, %v", versions, err)
	}
}

// A sync client's upload by node id: the file keeps its place and name,
// even renamed meanwhile; the final response carries its new ETag; a base
// gone stale by the end discards the upload with its reservation.
func TestUploadByNodeID(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.users["alice"]
	f, err := e.files.CreateFile(ctx, alice, e.root.ID, "a.txt", strings.NewReader("base"), 4)
	if err != nil {
		t.Fatal(err)
	}
	_, url := e.create(t, 6, http.StatusCreated, "nodeId", f.ID, "ifMatch", f.ETag, "mtime", "2021-06-07T08:09:10.5+03:00", "filename", "ignored.txt", "onConflict", "bogus")
	if resp := e.patch(t, "alice", url, 0, "edi"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first chunk: %d", resp.StatusCode)
	}
	if _, err := e.files.Rename(ctx, alice, f.ID, "renamed.txt"); err != nil {
		t.Fatal(err)
	}
	resp := e.patch(t, "alice", url, 3, "ted")
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get(NodeIDHeader) != f.ID {
		t.Fatalf("finish: %d %v", resp.StatusCode, resp.Header)
	}
	n, err := e.files.Get(ctx, alice, f.ID)
	if err != nil || n.Name != "renamed.txt" || n.Size != 6 || n.ETag == f.ETag || resp.Header.Get(NodeETagHeader) != n.ETag ||
		!n.MTime.Equal(time.Date(2021, 6, 7, 5, 9, 10, 5e8, time.UTC)) {
		t.Fatalf("file %+v, %v, ETag header %q", n, err, resp.Header.Get(NodeETagHeader))
	}

	// Two clients from the same base: the second is told at once.
	resp, _ = e.create(t, 1, http.StatusPreconditionFailed, "nodeId", f.ID, "ifMatch", f.ETag)
	if errorCode(t, resp) != "precondition_failed" {
		t.Fatal("stale at creation: wrong code")
	}
	// Changed after the upload began: refused at the end, and discarded.
	_, url = e.create(t, 2, http.StatusCreated, "nodeId", f.ID, "ifMatch", `"`+n.ETag+`"`)
	if _, err := e.files.WriteFile(ctx, alice, f.ID, strings.NewReader("web"), 3, fs.PutCond{}); err != nil {
		t.Fatal(err)
	}
	resp = e.patch(t, "alice", url, 0, "zz")
	if resp.StatusCode != http.StatusPreconditionFailed || errorCode(t, resp) != "precondition_failed" {
		t.Fatalf("stale at finish: %d", resp.StatusCode)
	}
	if resp := e.do(t, "alice", "HEAD", url, nil, ""); resp.StatusCode != http.StatusNotFound || e.reserved(t) != 0 {
		t.Fatalf("discarded upload: %d, reservations %d", resp.StatusCode, e.reserved(t))
	}
	if left, _ := filepath.Glob(filepath.Join(e.dataDir, "uploads", "tus", "*")); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
	if got, _ := e.files.Get(ctx, alice, f.ID); got.Size != 3 {
		t.Fatalf("overwritten: %+v", got)
	}
}

func TestUploadByNodeIDRejects(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f, err := e.files.CreateFile(ctx, e.users["alice"], e.root.ID, "a.txt", strings.NewReader("x"), 1)
	if err != nil {
		t.Fatal(err)
	}
	bobs, err := e.files.CreateFile(ctx, e.users["bob"], mustRoot(t, e, "bob"), "b.txt", strings.NewReader("x"), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		meta   []string
		status int
		code   string
	}{
		{"ifMatch alone", []string{"filename", "x", "ifMatch", f.ETag}, 400, "invalid_request"},
		{"a folder", []string{"nodeId", e.root.ID}, 400, "not_a_file"},
		{"someone else's", []string{"nodeId", bobs.ID}, 404, "not_found"},
		{"bad mtime", []string{"filename", "x", "mtime", "1700000000"}, 400, "invalid_request"},
		{"mtime before 1970", []string{"filename", "x", "mtime", "1969-12-31T23:59:59Z"}, 400, "invalid_request"},
		{"mtime far ahead", []string{"filename", "x", "mtime", time.Now().Add(48 * time.Hour).Format(time.RFC3339)}, 400, "invalid_request"},
	} {
		resp, _ := e.create(t, 1, tc.status, tc.meta...)
		if got := errorCode(t, resp); got != tc.code {
			t.Errorf("%s: code %q, want %q", tc.name, got, tc.code)
		}
	}
	if n := e.reserved(t); n != 0 {
		t.Errorf("%d reservations left by refused uploads", n)
	}
}

func mustRoot(t *testing.T, e env, user string) string {
	t.Helper()
	root, err := e.files.Root(context.Background(), e.users[user])
	if err != nil {
		t.Fatal(err)
	}
	return root.ID
}
