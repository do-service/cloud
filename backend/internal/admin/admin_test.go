package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/audit"
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
	mux      *http.ServeMux
	pool     *pgxpool.Pool
	files    *fs.Service
	users    *auth.Users
	sessions *auth.Sessions
	devices  *auth.Devices
	audit    *audit.Log
	dataDir  string
	queued   []string
	session  map[string]string
	ids      map[string]string
}

func newEnv(t *testing.T) *env {
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
	if _, err := pool.Exec(ctx, `TRUNCATE users, nodes, blobs, versions, groups, audit_log CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	e := &env{pool: pool, dataDir: t.TempDir(), users: auth.NewUsers(pool), audit: audit.New(pool),
		session: map[string]string{}, ids: map[string]string{}}
	store, err := localfs.Open(e.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e.files = fs.New(pool, store)
	e.sessions = auth.NewSessions(pool, time.Hour)
	e.devices = auth.NewDevices(pool)
	for _, u := range []struct {
		name  string
		admin bool
	}{{"root", true}, {"alice", false}} {
		created, err := e.users.CreateLocal(ctx, u.name, "", "password1", u.admin)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.files.EnsureRoot(ctx, created.ID); err != nil {
			t.Fatal(err)
		}
		e.ids[u.name] = created.ID
		e.login(t, u.name)
	}
	withUser := auth.Middleware(e.sessions, nil, auth.Cookies{})
	h := &Handlers{
		Users: e.users, Audit: e.audit, Devices: e.devices,
		SignedIn: func(next http.Handler) http.Handler { return withUser(auth.RequireUser(next)) },
		SetUp: func(ctx context.Context, id string) error {
			_, err := e.files.EnsureRoot(ctx, id)
			return err
		},
		Enqueue: func(_ context.Context, id string) error {
			e.queued = append(e.queued, id)
			return nil
		},
		System: System{PasswordLogin: true, EditorURL: "http://collabora:9980/hosting/discovery", AuditRetention: 365 * 24 * time.Hour,
			Editor: func(context.Context) (bool, error) { return true, nil }},
	}
	e.mux = http.NewServeMux()
	h.Register(e.mux)
	return e
}

func (e *env) login(t *testing.T, name string) {
	t.Helper()
	s, _, err := e.sessions.Create(context.Background(), e.ids[name], netip.Addr{}, "")
	if err != nil {
		t.Fatal(err)
	}
	e.session[name] = s
}

// as sends a request signed in as user; out, if set, gets the JSON body.
func (e *env) as(t *testing.T, user, method, path, body string, out any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: e.session[user]})
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if out != nil {
		_ = json.Unmarshal(rec.Body.Bytes(), out)
	}
	return rec
}

func want(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status || code != "" && !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("status = %d, body %s; want %d %s", rec.Code, rec.Body, status, code)
	}
}

func TestAdminsOnly(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/api/v1/admin/users", "/api/v1/admin/audit", "/api/v1/admin/system"} {
		want(t, e.as(t, "alice", "GET", path, "", nil), http.StatusForbidden, "forbidden")
	}
	want(t, e.as(t, "alice", "POST", "/api/v1/admin/users", `{"username":"x","password":"password1"}`, nil), http.StatusForbidden, "forbidden")
	var sys map[string]any
	want(t, e.as(t, "root", "GET", "/api/v1/admin/system", "", &sys), http.StatusOK, "")
	if sys["passwordLogin"] != true || sys["auditRetentionDays"] != 365.0 || sys["editor"].(map[string]any)["available"] != true {
		t.Fatalf("system = %v", sys)
	}
}

func TestManageUsers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	quota := int64(5 << 30)
	e.users.DefaultQuota = &quota

	var bob accountJSON
	want(t, e.as(t, "root", "POST", "/api/v1/admin/users", `{"username":"bob","displayName":"Боб","password":"password1"}`, &bob), http.StatusCreated, "")
	if bob.Username != "bob" || bob.DisplayName != "Боб" || bob.QuotaBytes == nil || *bob.QuotaBytes != quota || !bob.HasPassword || bob.IsAdmin {
		t.Fatalf("created %+v", bob)
	}
	if _, err := e.files.Root(ctx, bob.ID); err != nil {
		t.Fatalf("no root for the new user: %v", err)
	}
	var unlimited accountJSON
	want(t, e.as(t, "root", "POST", "/api/v1/admin/users", `{"username":"carol","password":"password1","quotaBytes":null}`, &unlimited), http.StatusCreated, "")
	if unlimited.QuotaBytes != nil {
		t.Fatalf("explicit null quota = %v", *unlimited.QuotaBytes)
	}
	want(t, e.as(t, "root", "POST", "/api/v1/admin/users", `{"username":"BOB","password":"password1"}`, nil), http.StatusConflict, "username_taken")
	want(t, e.as(t, "root", "POST", "/api/v1/admin/users", `{"username":"bad name","password":"password1"}`, nil), http.StatusBadRequest, "invalid_username")
	want(t, e.as(t, "root", "POST", "/api/v1/admin/users", `{"username":"dave","password":"short"}`, nil), http.StatusBadRequest, "weak_password")

	var page struct {
		Items      []accountJSON `json:"items"`
		NextCursor *string       `json:"nextCursor"`
	}
	want(t, e.as(t, "root", "GET", "/api/v1/admin/users?q=%D0%B1%D0%BE%D0%B1", "", &page), http.StatusOK, "")
	if len(page.Items) != 1 || page.Items[0].Username != "bob" || page.NextCursor != nil {
		t.Fatalf("search by name: %+v", page)
	}

	// Changes: name, quota (and back to none), admin, password.
	var changed accountJSON
	want(t, e.as(t, "root", "PATCH", "/api/v1/admin/users/"+bob.ID, `{"displayName":"Борис","quotaBytes":1024,"isAdmin":true}`, &changed), http.StatusOK, "")
	if changed.DisplayName != "Борис" || *changed.QuotaBytes != 1024 || !changed.IsAdmin {
		t.Fatalf("changed %+v", changed)
	}
	want(t, e.as(t, "root", "PATCH", "/api/v1/admin/users/"+bob.ID, `{"quotaBytes":null}`, &changed), http.StatusOK, "")
	if changed.QuotaBytes != nil {
		t.Fatal("quota not lifted")
	}
	want(t, e.as(t, "root", "PATCH", "/api/v1/admin/users/"+bob.ID, `{"quotaBytes":-1}`, nil), http.StatusBadRequest, "invalid_request")
	want(t, e.as(t, "root", "PATCH", "/api/v1/admin/users/"+bob.ID, `{"color":"red"}`, nil), http.StatusBadRequest, "invalid_request")
	want(t, e.as(t, "root", "PATCH", "/api/v1/admin/users/"+bob.ID, `{"status":"deleting"}`, nil), http.StatusBadRequest, "invalid_request")

	// Disabling and a new password end the user's sessions.
	e.ids["bob"] = bob.ID
	e.login(t, "bob")
	want(t, e.as(t, "root", "PATCH", "/api/v1/admin/users/"+bob.ID, `{"password":"new-password"}`, nil), http.StatusOK, "")
	if _, _, err := e.sessions.Lookup(ctx, e.session["bob"]); err == nil {
		t.Fatal("session survived a password reset")
	}
	if _, err := e.users.Authenticate(ctx, "bob", "new-password"); err != nil {
		t.Fatalf("new password: %v", err)
	}
	e.login(t, "alice")
	want(t, e.as(t, "root", "PATCH", "/api/v1/admin/users/"+e.ids["alice"], `{"status":"disabled"}`, nil), http.StatusOK, "")
	if _, _, err := e.sessions.Lookup(ctx, e.session["alice"]); err == nil {
		t.Fatal("session survived disabling")
	}
	want(t, e.as(t, "root", "PATCH", "/api/v1/admin/users/nope", `{"displayName":"x"}`, nil), http.StatusNotFound, "not_found")

	// The audit log has it all, the password never.
	var log struct {
		Items []auditJSON `json:"items"`
	}
	want(t, e.as(t, "root", "GET", "/api/v1/admin/audit?action=admin.", "", &log), http.StatusOK, "")
	if len(log.Items) != 6 || log.Items[0].Action != audit.UserUpdate || log.Items[0].Target.Name != "alice" || log.Items[0].Actor.Name != "root" {
		t.Fatalf("audit: %+v", log.Items)
	}
	if strings.Contains(e.as(t, "root", "GET", "/api/v1/admin/audit", "", nil).Body.String(), "new-password") {
		t.Fatal("a password in the audit log")
	}
}

func TestAdminSafeguards(t *testing.T) {
	e := newEnv(t)
	root := e.ids["root"]
	// Not to oneself: nobody else could undo it.
	want(t, e.as(t, "root", "PATCH", "/api/v1/admin/users/"+root, `{"isAdmin":false}`, nil), http.StatusConflict, "own_account")
	want(t, e.as(t, "root", "PATCH", "/api/v1/admin/users/"+root, `{"status":"disabled"}`, nil), http.StatusConflict, "own_account")
	want(t, e.as(t, "root", "DELETE", "/api/v1/admin/users/"+root, "", nil), http.StatusConflict, "own_account")
	want(t, e.as(t, "root", "PATCH", "/api/v1/admin/users/"+root, `{"displayName":"Главный"}`, nil), http.StatusOK, "")

	// Nor to the last active admin, by another admin who then steps down.
	var second accountJSON
	want(t, e.as(t, "root", "POST", "/api/v1/admin/users", `{"username":"second","password":"password1","isAdmin":true}`, &second), http.StatusCreated, "")
	e.ids["second"] = second.ID
	e.login(t, "second")
	want(t, e.as(t, "second", "PATCH", "/api/v1/admin/users/"+root, `{"isAdmin":false}`, nil), http.StatusOK, "")
	e.login(t, "root")
	want(t, e.as(t, "root", "GET", "/api/v1/admin/users", "", nil), http.StatusForbidden, "forbidden")
	if _, err := e.users.UpdateAccount(context.Background(), second.ID, auth.AccountChange{Status: ptr(auth.StatusDisabled)}); err != auth.ErrLastAdmin {
		t.Fatalf("disabling the last admin: %v", err)
	}
	if _, err := e.users.MarkDeleting(context.Background(), second.ID); err != auth.ErrLastAdmin {
		t.Fatalf("deleting the last admin: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// Deleting a user signs them out at once; the job then removes their
// files, what was shared with them, and the account.
func TestDeleteUser(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.ids["alice"]
	aliceRoot, _ := e.files.Root(ctx, alice)
	docs, err := e.files.CreateDir(ctx, alice, aliceRoot.ID, "Документы")
	if err != nil {
		t.Fatal(err)
	}
	f, err := e.files.CreateFile(ctx, alice, docs.ID, "a.txt", strings.NewReader("v1"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.files.PutFile(ctx, alice, docs.ID, "a.txt", strings.NewReader("v2"), 2, fs.PutCond{}); err != nil {
		t.Fatal(err)
	}
	trashed, _ := e.files.CreateFile(ctx, alice, aliceRoot.ID, "old.txt", strings.NewReader("x"), 1)
	if err := e.files.Trash(ctx, alice, trashed.ID); err != nil {
		t.Fatal(err)
	}
	// Root's folder, shared with Alice and locked by her.
	rootsRoot, _ := e.files.Root(ctx, e.ids["root"])
	shared, _ := e.files.CreateDir(ctx, e.ids["root"], rootsRoot.ID, "Общая")
	shares := share.New(e.pool, e.files)
	if _, err := shares.Grant(ctx, e.ids["root"], shared.ID, share.GranteeUser, alice, access.Read|access.Write); err != nil {
		t.Fatal(err)
	}
	if _, err := e.files.Lock(ctx, alice, f.ID, fs.LockRequest{Kind: fs.LockWebDAV}); err != nil {
		t.Fatal(err)
	}

	want(t, e.as(t, "root", "DELETE", "/api/v1/admin/users/"+alice, "", nil), http.StatusAccepted, "")
	if len(e.queued) != 1 || e.queued[0] != alice {
		t.Fatalf("queued %v", e.queued)
	}
	if _, _, err := e.sessions.Lookup(ctx, e.session["alice"]); err == nil {
		t.Fatal("still signed in")
	}
	if _, err := e.users.Authenticate(ctx, "alice", "password1"); err == nil {
		t.Fatal("still signs in")
	}
	// Being deleted: no changes meanwhile.
	want(t, e.as(t, "root", "PATCH", "/api/v1/admin/users/"+alice, `{"status":"active"}`, nil), http.StatusNotFound, "not_found")

	w := &DeleteUserWorker{FS: e.files, Users: e.users}
	if err := w.Work(ctx, &river.Job[DeleteUserArgs]{Args: DeleteUserArgs{UserID: alice}}); err != nil {
		t.Fatal(err)
	}
	var nodes, blobs, grants, users int
	if err := e.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM nodes WHERE owner_id = $1),
		(SELECT count(*) FROM blobs),
		(SELECT count(*) FROM shares WHERE grantee_id = $1),
		(SELECT count(*) FROM users WHERE id = $1)`, alice).Scan(&nodes, &blobs, &grants, &users); err != nil {
		t.Fatal(err)
	}
	if nodes != 0 || blobs != 0 || grants != 0 || users != 0 {
		t.Fatalf("left: %d nodes, %d blobs, %d shares, %d users", nodes, blobs, grants, users)
	}
	left, err := os.ReadDir(e.dataDir + "/files")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range left {
		if d.Name() == alice {
			t.Fatal("the user's directory is still on disk")
		}
	}
	for _, sub := range []string{"trash", "versions"} {
		if entries, _ := os.ReadDir(e.dataDir + "/" + sub); len(entries) != 0 {
			t.Fatalf("%s not empty: %v", sub, entries)
		}
	}
	// Run again (a retried job): nothing left, nothing fails.
	if err := w.Work(ctx, &river.Job[DeleteUserArgs]{Args: DeleteUserArgs{UserID: alice}}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var log struct {
		Items []auditJSON `json:"items"`
	}
	want(t, e.as(t, "root", "GET", "/api/v1/admin/audit?action=admin.user.delete", "", &log), http.StatusOK, "")
	if len(log.Items) != 1 || log.Items[0].Target.Name != "alice" {
		t.Fatalf("audit: %+v", log.Items)
	}
}

// An admin sees a user's devices and revokes one; the token stops
// working at once, and the user's other devices stay.
func TestUserDevices(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.ids["alice"]
	laptop, laptopToken, err := e.devices.Create(ctx, alice, "Ноутбук", "macos", "1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.devices.Create(ctx, alice, "Рабочий", "windows", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.devices.Create(ctx, e.ids["root"], "Свой", "linux", ""); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/admin/users/" + alice + "/devices"

	want(t, e.as(t, "alice", "GET", base, "", nil), http.StatusForbidden, "forbidden")
	want(t, e.as(t, "alice", "DELETE", base+"/"+laptop.ID, "", nil), http.StatusForbidden, "forbidden")

	var list []auth.Device
	want(t, e.as(t, "root", "GET", base, "", &list), http.StatusOK, "")
	if len(list) != 2 || list[1].ID != laptop.ID || list[1].Platform != "macos" || list[1].ClientVersion != "1.2.0" {
		t.Fatalf("devices: %+v", list)
	}
	var none []auth.Device
	want(t, e.as(t, "root", "GET", "/api/v1/admin/users/"+e.ids["root"]+"/devices", "", &none), http.StatusOK, "")
	if len(none) != 1 || none[0].Name != "Свой" {
		t.Fatalf("root's devices: %+v", none)
	}
	want(t, e.as(t, "root", "GET", "/api/v1/admin/users/nope/devices", "", nil), http.StatusNotFound, "not_found")
	want(t, e.as(t, "root", "GET", "/api/v1/admin/users/00000000-0000-0000-0000-000000000000/devices", "", nil), http.StatusNotFound, "not_found")

	// Another user's device under Alice's address is not hers.
	rootDevices, _ := e.devices.List(ctx, e.ids["root"])
	want(t, e.as(t, "root", "DELETE", base+"/"+rootDevices[0].ID, "", nil), http.StatusNotFound, "not_found")
	want(t, e.as(t, "root", "DELETE", base+"/nope", "", nil), http.StatusNotFound, "not_found")

	want(t, e.as(t, "root", "DELETE", base+"/"+laptop.ID, "", nil), http.StatusNoContent, "")
	if _, _, err := e.devices.Lookup(ctx, laptopToken, netip.Addr{}); err != auth.ErrNoDevice {
		t.Fatalf("revoked token: %v", err)
	}
	want(t, e.as(t, "root", "GET", base, "", &list), http.StatusOK, "")
	if len(list) != 1 || list[0].Name != "Рабочий" {
		t.Fatalf("left: %+v", list)
	}
	want(t, e.as(t, "root", "DELETE", base+"/"+laptop.ID, "", nil), http.StatusNotFound, "not_found")

	var log struct {
		Items []auditJSON `json:"items"`
	}
	want(t, e.as(t, "root", "GET", "/api/v1/admin/audit?action=device.revoke", "", &log), http.StatusOK, "")
	if len(log.Items) != 1 || log.Items[0].Actor.Name != "root" || log.Items[0].Target.Name != "Ноутбук" || log.Items[0].Details["user"] != "alice" {
		t.Fatalf("audit: %+v", log.Items)
	}
}

func TestAuditLogPages(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.7:5555"
	for i := range 60 {
		e.audit.Record(r, audit.Event{Actor: audit.Actor{ID: e.ids["root"], Name: "root"}, Action: audit.UserUpdate,
			TargetID: e.ids["alice"], TargetName: "alice", Details: map[string]any{"n": i}})
	}
	r.RemoteAddr = "198.51.100.9:5555"
	e.audit.Record(r, audit.Event{Actor: audit.Actor{Name: "Ghost"}, Action: audit.LoginFailed})
	e.audit.Record(r, audit.Event{Actor: audit.Actor{ID: e.ids["alice"], Name: "alice"}, Action: audit.ShareGrant,
		TargetID: "n1", TargetName: "Отчёт 50%.docx"})
	if _, err := e.pool.Exec(ctx, `UPDATE audit_log SET at = '2026-01-15T12:00:00Z' WHERE action = $1`, audit.LoginFailed); err != nil {
		t.Fatal(err)
	}

	type page struct {
		Items []auditJSON `json:"items"`
		Total int64       `json:"total"`
		Page  int         `json:"page"`
		Size  int         `json:"size"`
	}
	var p page
	want(t, e.as(t, "root", "GET", "/api/v1/admin/audit", "", &p), http.StatusOK, "")
	if len(p.Items) != 50 || p.Total != 62 || p.Page != 1 || p.Size != 50 || p.Items[0].Action != audit.ShareGrant {
		t.Fatalf("default page: %d items, total %d, page %d, size %d", len(p.Items), p.Total, p.Page, p.Size)
	}
	want(t, e.as(t, "root", "GET", "/api/v1/admin/audit?page=3&size=25", "", &p), http.StatusOK, "")
	if len(p.Items) != 12 || p.Total != 62 || p.Page != 3 || p.Size != 25 || p.Items[11].Details["n"] != 0.0 {
		t.Fatalf("last page: %d items, total %d", len(p.Items), p.Total)
	}
	p = page{}
	want(t, e.as(t, "root", "GET", "/api/v1/admin/audit?page=4&size=25", "", &p), http.StatusOK, "")
	if p.Items == nil || len(p.Items) != 0 || p.Total != 62 {
		t.Fatalf("past the end: %+v", p)
	}

	for _, tc := range []struct {
		query string
		total int64
	}{
		{"action=share.", 1},
		{"action=admin.user.update", 60},
		// By login, any case, even with no account behind it.
		{"actor=ghost", 1},
		{"actor=ROOT", 60},
		{"from=2026-01-15T00:00:00%2B03:00&to=2026-01-16T00:00:00%2B03:00", 1},
		{"to=2026-01-15T11:00:00Z", 0},
		{"q=50%25", 1},
		{"q=" + e.ids["alice"], 60},
		{"ip=198.51.100.9", 2},
		{"ip=192.0.2.0/24", 60},
		{"ip=::ffff:192.0.2.7", 60},
		{"actor=alice&q=отчёт&ip=198.51.100.0/24&action=share.grant", 1},
	} {
		p = page{}
		want(t, e.as(t, "root", "GET", "/api/v1/admin/audit?"+tc.query, "", &p), http.StatusOK, "")
		if p.Total != tc.total {
			t.Errorf("%s: total %d, want %d", tc.query, p.Total, tc.total)
		}
	}

	for _, q := range []string{"size=10", "size=x", "page=0", "page=-1", "page=x", "page=99999999999",
		"from=yesterday", "to=2026-01-15", "ip=nope", "ip=10.0.0.0/33"} {
		want(t, e.as(t, "root", "GET", "/api/v1/admin/audit?"+q, "", nil), http.StatusBadRequest, "invalid_request")
	}
}
