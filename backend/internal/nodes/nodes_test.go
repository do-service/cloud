package nodes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/db"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/ratelimit"
	"github.com/do-service/cloud/backend/internal/share"
	"github.com/do-service/cloud/backend/internal/storage"
	"github.com/do-service/cloud/backend/internal/storage/localfs"
	"github.com/do-service/cloud/backend/internal/upload"
)

func testDatabaseURL() string {
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
}

type env struct {
	mux  *http.ServeMux
	svc  *fs.Service
	pool *pgxpool.Pool
	// dataDir is the storage root, for staging upload content.
	dataDir  string
	sessions *auth.Sessions
	root     fs.Node
	// session is alice's session cookie value.
	session string
	user    string
}

// newEnv serves alice's tree; wrap, if set, wraps the storage backend (to
// inject failures).
// testJobs, when a test sets it, runs background copies.
var testJobs CopyJobs

func newEnv(t *testing.T, wrap func(storage.Backend) storage.Backend) env {
	t.Helper()
	url := testDatabaseURL()
	if err := db.Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, url, 8, 0, 10*time.Second)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE users, nodes, blobs, versions CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	dataDir := t.TempDir()
	store, err := localfs.Open(dataDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	var backend storage.Backend = store
	if wrap != nil {
		backend = wrap(store)
	}
	e := env{mux: http.NewServeMux(), svc: fs.New(pool, backend), pool: pool, dataDir: dataDir}
	if err := pool.QueryRow(ctx, `INSERT INTO users (username) VALUES ('alice') RETURNING id`).Scan(&e.user); err != nil {
		t.Fatal(err)
	}
	if e.root, err = e.svc.EnsureRoot(ctx, e.user); err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewSessions(pool, time.Hour)
	if e.session, _, err = sessions.Create(ctx, e.user, netip.Addr{}, ""); err != nil {
		t.Fatal(err)
	}
	withUser := auth.Middleware(sessions, nil, auth.Cookies{})
	e.sessions = sessions
	signedIn := func(next http.Handler) http.Handler { return withUser(auth.RequireUser(next)) }
	shares := share.New(pool, e.svc)
	box, err := auth.NewSecretBox([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	uploads, err := upload.New(e.svc, dataDir, signedIn)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{
		FS: e.svc, Shares: shares, Links: share.NewLinks(shares, box), Uploads: uploads,
		// Three tries an hour: the limit shows within a test.
		UnlockLimiter: ratelimit.New(time.Hour, 3),
		SignedIn:      signedIn,
		Jobs:          testJobs,
	}
	h.Register(e.mux)
	return e
}

func (e env) get(t *testing.T, path string, signedIn bool) (*httptest.ResponseRecorder, childrenResponse) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if signedIn {
		req.AddCookie(&http.Cookie{Name: "session", Value: e.session})
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	var resp childrenResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	return rec, resp
}

func TestChildren(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs, err := e.svc.CreateDir(ctx, e.user, e.root.ID, "Документы")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"b.txt", "a.txt", "c.txt"} {
		if _, err := e.svc.CreateFile(ctx, e.user, e.root.ID, name, strings.NewReader(name), int64(len(name))); err != nil {
			t.Fatal(err)
		}
	}

	if rec, _ := e.get(t, "/api/v1/nodes/root/children", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d", rec.Code)
	}
	rec, page := e.get(t, "/api/v1/nodes/root/children?limit=2", true)
	if rec.Code != http.StatusOK || len(page.Items) != 2 || page.NextCursor == nil {
		t.Fatalf("first page: %d %s", rec.Code, rec.Body)
	}
	if it := page.Items[0]; it.ID != docs.ID || it.Kind != fs.KindDir || it.Name != "Документы" || it.ETag == "" || strings.Contains(rec.Body.String(), `"mime":""`) {
		t.Fatalf("folder item: %s", rec.Body)
	}
	if page.Items[1].Name != "a.txt" || page.Items[1].MIME != "text/plain; charset=utf-8" || page.Items[1].Size != 5 {
		t.Fatalf("file item: %+v", page.Items[1])
	}
	rec, page = e.get(t, "/api/v1/nodes/"+e.root.ID+"/children?limit=2&cursor="+url.QueryEscape(*page.NextCursor), true)
	if rec.Code != http.StatusOK || len(page.Items) != 2 || page.Items[0].Name != "b.txt" || page.NextCursor != nil || !strings.Contains(rec.Body.String(), `"nextCursor":null`) {
		t.Fatalf("last page: %d %s", rec.Code, rec.Body)
	}
	rec, page = e.get(t, "/api/v1/nodes/root/children?sort=size&order=desc", true)
	if rec.Code != http.StatusOK || len(page.Items) != 4 || page.Items[0].Kind != fs.KindDir {
		t.Fatalf("by size desc: %d %s", rec.Code, rec.Body)
	}
	// An empty folder lists as [], not null.
	if rec, _ := e.get(t, "/api/v1/nodes/"+docs.ID+"/children", true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty folder: %d %s", rec.Code, rec.Body)
	}

	_, first := e.get(t, "/api/v1/nodes/root/children?limit=1", true)
	file := page.Items[1]
	for _, tc := range []struct {
		path string
		code int
		err  string
	}{
		{"/api/v1/nodes/" + file.ID + "/children", http.StatusBadRequest, "not_a_directory"},
		{"/api/v1/nodes/00000000-0000-0000-0000-000000000000/children", http.StatusNotFound, "not_found"},
		{"/api/v1/nodes/nope/children", http.StatusNotFound, "not_found"},
		{"/api/v1/nodes/root/children?sort=owner", http.StatusBadRequest, "invalid_request"},
		{"/api/v1/nodes/root/children?order=up", http.StatusBadRequest, "invalid_request"},
		{"/api/v1/nodes/root/children?limit=0", http.StatusBadRequest, "invalid_request"},
		{"/api/v1/nodes/root/children?cursor=xyz", http.StatusBadRequest, "invalid_request"},
		{"/api/v1/nodes/root/children?sort=mtime&cursor=" + url.QueryEscape(*first.NextCursor), http.StatusBadRequest, "invalid_request"},
	} {
		rec, _ := e.get(t, tc.path, true)
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), `"code":"`+tc.err+`"`) {
			t.Errorf("%s: %d %s", tc.path, rec.Code, rec.Body)
		}
	}
}
