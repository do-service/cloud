package wopi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
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

const hostURL = "http://cloud:8080"

func testDatabaseURL() string {
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
}

type env struct {
	h       *Handlers
	mux     *http.ServeMux
	svc     *fs.Service
	shares  *share.Service
	pool    *pgxpool.Pool
	key     *rsa.PrivateKey
	oldKey  *rsa.PrivateKey
	now     time.Time
	ids     map[string]string
	session map[string]string
	root    map[string]fs.Node
	// discovery is what the fake editor serves, fetches how often it was
	// asked.
	discovery atomic.Pointer[string]
	fetches   atomic.Int32
}

// rotate makes the fake editor publish key, with old as the old one.
func (e *env) rotate(key, old *rsa.PrivateKey) {
	doc := fakeDiscovery(key, old)
	e.discovery.Store(&doc)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func fakeDiscovery(key, old *rsa.PrivateKey) string {
	exp := func(k *rsa.PrivateKey) string { return b64(big.NewInt(int64(k.E)).Bytes()) }
	return `<?xml version="1.0" encoding="utf-8"?><wopi-discovery><net-zone name="external-http">` +
		`<app name="writer"><action default="true" ext="docx" name="edit" urlsrc="http://localhost:9980/browser/x/cool.html?"/>` +
		`<action ext="odt" name="edit" urlsrc="http://localhost:9980/browser/x/cool.html?&lt;ui=UI_LLCC&amp;&gt;"/></app>` +
		`<app name="draw"><action ext="pdf" name="view_comment" urlsrc="http://localhost:9980/browser/x/cool.html?"/></app>` +
		`</net-zone><proof-key value="x" modulus="` + b64(key.N.Bytes()) + `" exponent="` + exp(key) +
		`" oldvalue="y" oldmodulus="` + b64(old.N.Bytes()) + `" oldexponent="` + exp(old) + `"/></wopi-discovery>`
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
	if _, err := pool.Exec(ctx, `TRUNCATE users, nodes, blobs, versions, groups CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	oldKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{svc: fs.New(pool, store), pool: pool, key: key, oldKey: oldKey, now: time.Now(),
		ids: map[string]string{}, session: map[string]string{}, root: map[string]fs.Node{}}
	e.rotate(key, oldKey)
	editor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.fetches.Add(1)
		_, _ = w.Write([]byte(*e.discovery.Load()))
	}))
	t.Cleanup(editor.Close)

	e.shares = share.New(pool, e.svc)
	sessions := auth.NewSessions(pool, time.Hour)
	for _, name := range []string{"alice", "bob"} {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO users (username, display_name) VALUES ($1, $2) RETURNING id`, name, strings.ToUpper(name[:1])+name[1:]).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if e.root[name], err = e.svc.EnsureRoot(ctx, id); err != nil {
			t.Fatal(err)
		}
		if e.session[name], _, err = sessions.Create(ctx, id, netip.Addr{}, ""); err != nil {
			t.Fatal(err)
		}
		e.ids[name] = id
	}
	withUser := auth.Middleware(sessions, nil, auth.Cookies{})
	e.h = &Handlers{
		FS: e.svc, Tokens: NewTokens(pool), Discovery: NewDiscovery(editor.URL, ""), HostURL: hostURL,
		PublicOrigin: "http://localhost:8090", SignedIn: func(next http.Handler) http.Handler { return withUser(auth.RequireUser(next)) },
		now: func() time.Time { return e.now },
	}
	e.mux = http.NewServeMux()
	e.h.Register(e.mux)
	return e
}

func (e *env) file(t *testing.T, user, name, content string) fs.Node {
	t.Helper()
	n, err := e.svc.CreateFile(context.Background(), e.ids[user], e.root[user].ID, name, strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// open asks the API to edit a file, as the web UI does.
func (e *env) open(t *testing.T, user, id string) (*httptest.ResponseRecorder, editResponse) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/nodes/"+id+"/edit", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: e.session[user]})
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	var res editResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	return rec, res
}

func (e *env) token(t *testing.T, user, id string) string {
	t.Helper()
	rec, res := e.open(t, user, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body)
	}
	return res.AccessToken
}

// call makes a WOPI request signed as the editor would, with key (nil:
// unsigned) at the env's time; hdr are name, value pairs.
func (e *env) call(t *testing.T, key *rsa.PrivateKey, method, path, token, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	target := path + "?access_token=" + url.QueryEscape(token)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	if key != nil {
		ticks := e.now.UnixNano()/100 + ticksAtUnixEpoch
		digest := sha256.Sum256(proofData(token, hostURL+req.URL.RequestURI(), ticks))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-WOPI-TimeStamp", strconv.FormatInt(ticks, 10))
		req.Header.Set("X-WOPI-Proof", b64(sig))
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func code(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; headers %v body %q", rec.Code, want, rec.Header(), rec.Body.String())
	}
}

func TestEditAndFileInfo(t *testing.T) {
	e := newEnv(t)
	doc := e.file(t, "alice", "Отчёт.docx", "v1")
	pdf := e.file(t, "alice", "a.pdf", "%PDF")
	txt := e.file(t, "alice", "a.bin", "x")

	rec, res := e.open(t, "alice", doc.ID)
	code(t, rec, http.StatusOK)
	if res.URL != "http://localhost:9980/browser/x/cool.html?WOPISrc="+url.QueryEscape(hostURL+"/wopi/files/"+doc.ID)+"&lang=ru" ||
		res.AccessToken == "" || time.Until(time.UnixMilli(res.AccessTokenTTL)) < 9*time.Hour {
		t.Fatalf("edit = %+v", res)
	}
	if rec, _ := e.open(t, "alice", txt.ID); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not_editable") {
		t.Fatalf("unsupported type: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := e.open(t, "bob", doc.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("someone else's file: %d", rec.Code)
	}

	info := e.call(t, e.key, "GET", "/wopi/files/"+doc.ID, res.AccessToken, "")
	code(t, info, http.StatusOK)
	var fi map[string]any
	if err := json.Unmarshal(info.Body.Bytes(), &fi); err != nil {
		t.Fatal(err)
	}
	if fi["BaseFileName"] != "Отчёт.docx" || fi["Size"] != 2.0 || fi["UserCanWrite"] != true || fi["UserFriendlyName"] != "Alice" ||
		fi["Version"] != doc.ETag || fi["SupportsLocks"] != true || fi["UserCanNotWriteRelative"] != false || fi["PostMessageOrigin"] != "http://localhost:8090" ||
		fi["ClosePostMessage"] != true {
		t.Fatalf("CheckFileInfo = %v", fi)
	}
	get := e.call(t, e.key, "GET", "/wopi/files/"+doc.ID+"/contents", res.AccessToken, "")
	code(t, get, http.StatusOK)
	if get.Body.String() != "v1" || get.Header().Get("X-WOPI-ItemVersion") != doc.ETag {
		t.Fatalf("GetFile = %q", get.Body.String())
	}

	// A token is for its file, while it lives.
	code(t, e.call(t, e.key, "GET", "/wopi/files/"+pdf.ID, res.AccessToken, ""), http.StatusUnauthorized)
	code(t, e.call(t, e.key, "GET", "/wopi/files/"+doc.ID, "nope", ""), http.StatusUnauthorized)
	if _, err := e.pool.Exec(context.Background(), `UPDATE wopi_tokens SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	code(t, e.call(t, e.key, "GET", "/wopi/files/"+doc.ID, res.AccessToken, ""), http.StatusUnauthorized)

	// The web UI learns what opens.
	req := httptest.NewRequest("GET", "/api/v1/editor", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: e.session["alice"]})
	ed := httptest.NewRecorder()
	e.mux.ServeHTTP(ed, req)
	if strings.TrimSpace(ed.Body.String()) != `{"extensions":{"docx":"edit","odt":"edit","pdf":"view"}}` {
		t.Fatalf("editor info: %s", ed.Body)
	}
	if got := e.h.Discovery.Origin(); got != "http://localhost:9980" {
		t.Fatalf("editor origin = %q", got)
	}
}

func TestProof(t *testing.T) {
	e := newEnv(t)
	doc := e.file(t, "alice", "a.docx", "v1")
	tok := e.token(t, "alice", doc.ID)
	path := "/wopi/files/" + doc.ID
	code(t, e.call(t, e.key, "GET", path, tok, ""), http.StatusOK)
	// Across a key rotation, the old key still signs.
	code(t, e.call(t, e.oldKey, "GET", path, tok, ""), http.StatusOK)
	// The spec answers a bad proof with 500.
	code(t, e.call(t, nil, "GET", path, tok, ""), http.StatusInternalServerError)
	stranger, _ := rsa.GenerateKey(rand.Reader, 2048)
	code(t, e.call(t, stranger, "GET", path, tok, ""), http.StatusInternalServerError)
	rec := e.call(t, e.key, "GET", path, tok, "")
	e.now = e.now.Add(21 * time.Minute)
	code(t, rec, http.StatusOK)
	stale := httptest.NewRequest("GET", path+"?access_token="+url.QueryEscape(tok), nil)
	ticks := (e.now.Add(-21*time.Minute)).UnixNano()/100 + ticksAtUnixEpoch
	digest := sha256.Sum256(proofData(tok, hostURL+stale.URL.RequestURI(), ticks))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, e.key, crypto.SHA256, digest[:])
	stale.Header.Set("X-WOPI-TimeStamp", strconv.FormatInt(ticks, 10))
	stale.Header.Set("X-WOPI-Proof", b64(sig))
	old := httptest.NewRecorder()
	e.mux.ServeHTTP(old, stale)
	code(t, old, http.StatusInternalServerError)
}

// After the editor rotates its key, a request signed with the new one
// refetches the cached discovery, but bad proofs can't make it fetch more
// than once per interval.
func TestProofKeyRotation(t *testing.T) {
	e := newEnv(t)
	doc := e.file(t, "alice", "a.docx", "v1")
	tok := e.token(t, "alice", doc.ID)
	path := "/wopi/files/" + doc.ID
	code(t, e.call(t, e.key, "GET", path, tok, ""), http.StatusOK)
	fetches := e.fetches.Load()

	// Within the interval, forged requests and even the new key wait.
	newKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	e.rotate(newKey, e.key)
	stranger, _ := rsa.GenerateKey(rand.Reader, 2048)
	for range 5 {
		code(t, e.call(t, stranger, "GET", path, tok, ""), http.StatusInternalServerError)
	}
	code(t, e.call(t, newKey, "GET", path, tok, ""), http.StatusInternalServerError)
	if got := e.fetches.Load(); got != fetches {
		t.Fatalf("discovery fetched %d times within the interval", got-fetches)
	}

	e.h.Discovery.refetchEvery = 0
	code(t, e.call(t, newKey, "GET", path, tok, ""), http.StatusOK)
	if got := e.fetches.Load(); got != fetches+1 {
		t.Fatalf("discovery fetched %d times, want 1", got-fetches)
	}
	// The key it replaced is still accepted as the old one, without
	// fetching again.
	code(t, e.call(t, e.key, "GET", path, tok, ""), http.StatusOK)
	if got := e.fetches.Load(); got != fetches+1 {
		t.Fatalf("discovery fetched %d times, want 1", got-fetches)
	}
}

func TestSaveAndLocks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	doc := e.file(t, "alice", "a.docx", "v1")
	tok := e.token(t, "alice", doc.ID)
	path := "/wopi/files/" + doc.ID
	put := func(body, lock string, hdr ...string) *httptest.ResponseRecorder {
		return e.call(t, e.key, "POST", path+"/contents", tok, body, append([]string{"X-WOPI-Override", "PUT", "X-WOPI-Lock", lock}, hdr...)...)
	}
	lockOp := func(op, lock string, hdr ...string) *httptest.ResponseRecorder {
		return e.call(t, e.key, "POST", path, tok, "", append([]string{"X-WOPI-Override", op, "X-WOPI-Lock", lock}, hdr...)...)
	}

	// Unlocked, a save goes through as a new version.
	rec := put("v2", "")
	code(t, rec, http.StatusOK)
	var saved map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &saved)
	if versions, _, err := e.svc.ListVersions(ctx, e.ids["alice"], doc.ID); err != nil || len(versions) != 1 || saved["LastModifiedTime"] == "" {
		t.Fatalf("versions %v %v, response %v", versions, err, saved)
	}
	// Collabora's timestamp: changed meanwhile, the save is refused.
	rec = put("v3", "", "X-COOL-WOPI-Timestamp", "2001-01-01T00:00:00Z")
	code(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), `"COOLStatusCode":1010`) {
		t.Fatalf("timestamp conflict: %s", rec.Body)
	}
	code(t, put("v3", "", "X-COOL-WOPI-Timestamp", saved["LastModifiedTime"]), http.StatusOK)

	code(t, lockOp("LOCK", "A"), http.StatusOK)
	code(t, lockOp("LOCK", "A"), http.StatusOK) // refresh
	rec = lockOp("LOCK", "B")
	code(t, rec, http.StatusConflict)
	if rec.Header().Get("X-WOPI-Lock") != "A" {
		t.Fatalf("conflict lock = %q", rec.Header().Get("X-WOPI-Lock"))
	}
	code(t, put("v4", "B"), http.StatusConflict)
	code(t, put("v4", ""), http.StatusConflict)
	code(t, put("v4", "A"), http.StatusOK)
	if got := lockOp("GET_LOCK", "").Header().Get("X-WOPI-Lock"); got != "A" {
		t.Fatalf("GET_LOCK = %q", got)
	}
	code(t, lockOp("REFRESH_LOCK", "A"), http.StatusOK)
	code(t, lockOp("REFRESH_LOCK", "B"), http.StatusConflict)
	// Locked from the editor: not from the web UI or WebDAV.
	if _, err := e.svc.Rename(ctx, e.ids["alice"], doc.ID, "b.docx"); err == nil {
		t.Fatal("renamed a file open in the editor")
	}
	// Someone editing the same document saves with the same lock.
	if _, err := e.shares.Grant(ctx, e.ids["alice"], doc.ID, share.GranteeUser, e.ids["bob"], access.Read|access.Write); err != nil {
		t.Fatal(err)
	}
	bobs := e.token(t, "bob", doc.ID)
	code(t, e.call(t, e.key, "POST", path+"/contents", bobs, "bob", "X-WOPI-Override", "PUT", "X-WOPI-Lock", "A"), http.StatusOK)

	code(t, lockOp("LOCK", "C", "X-WOPI-OldLock", "B"), http.StatusConflict)
	code(t, lockOp("LOCK", "C", "X-WOPI-OldLock", "A"), http.StatusOK)
	rec = lockOp("UNLOCK", "A")
	code(t, rec, http.StatusConflict)
	if rec.Header().Get("X-WOPI-Lock") != "C" {
		t.Fatalf("unlock conflict = %q", rec.Header().Get("X-WOPI-Lock"))
	}
	code(t, lockOp("UNLOCK", "C"), http.StatusOK)
	if got := lockOp("GET_LOCK", ""); got.Header().Get("X-WOPI-Lock") != "" || got.Code != http.StatusOK {
		t.Fatalf("GET_LOCK after unlock = %q", got.Header().Get("X-WOPI-Lock"))
	}

	// A WebDAV client's lock is in the editor's way too, with no WOPI
	// lock string to show.
	l, err := e.svc.Lock(ctx, e.ids["alice"], doc.ID, fs.LockRequest{Kind: fs.LockWebDAV})
	if err != nil {
		t.Fatal(err)
	}
	rec = lockOp("LOCK", "D")
	code(t, rec, http.StatusConflict)
	if v, ok := rec.Header()["X-Wopi-Lock"]; !ok || v[0] != "" {
		t.Fatalf("WebDAV lock shown as %v", rec.Header())
	}
	code(t, put("v5", ""), http.StatusConflict)
	if err := e.svc.Unlock(ctx, e.ids["alice"], l.Token); err != nil {
		t.Fatal(err)
	}

	// Revoked: the token no longer opens it.
	sh, err := e.shares.List(ctx, e.ids["alice"], doc.ID)
	if err != nil || len(sh) != 1 {
		t.Fatalf("shares %v %v", sh, err)
	}
	if err := e.shares.Revoke(ctx, e.ids["alice"], sh[0].ID); err != nil {
		t.Fatal(err)
	}
	code(t, e.call(t, e.key, "GET", path, bobs, ""), http.StatusNotFound)
}

func TestPutRelativeAndRename(t *testing.T) {
	e := newEnv(t)
	doc := e.file(t, "alice", "Отчёт.docx", "v1")
	tok := e.token(t, "alice", doc.ID)
	path := "/wopi/files/" + doc.ID
	relative := func(hdr ...string) *httptest.ResponseRecorder {
		return e.call(t, e.key, "POST", path, tok, "pdf", append([]string{"X-WOPI-Override", "PUT_RELATIVE"}, hdr...)...)
	}
	var out map[string]string
	for _, want := range []string{"Отчёт.pdf", "Отчёт (2).pdf"} {
		rec := relative("X-WOPI-SuggestedTarget", ".pdf")
		code(t, rec, http.StatusOK)
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out["Name"] != want {
			t.Fatalf("PutRelativeFile = %v, want %s", out, want)
		}
	}
	// The new file opens with the token in its URL.
	u, err := url.Parse(out["Url"])
	if err != nil || !strings.HasPrefix(out["Url"], hostURL+"/wopi/files/") {
		t.Fatalf("Url = %q", out["Url"])
	}
	code(t, e.call(t, e.key, "GET", u.Path, u.Query().Get("access_token"), ""), http.StatusOK)

	// "Копия.docx", UTF-7; taken without overwriting is a conflict.
	name := "+BBoEPgQ/BDgETw-.docx"
	code(t, relative("X-WOPI-RelativeTarget", name), http.StatusOK)
	code(t, relative("X-WOPI-RelativeTarget", name), http.StatusConflict)
	code(t, relative("X-WOPI-RelativeTarget", name, "X-WOPI-OverwriteRelativeTarget", "true"), http.StatusOK)

	rec := e.call(t, e.key, "POST", path, tok, "", "X-WOPI-Override", "RENAME_FILE", "X-WOPI-RequestedName", "+BB0EPgQyBEsEOQ-")
	code(t, rec, http.StatusOK)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["Name"] != "Новый" {
		t.Fatalf("RenameFile = %v", out)
	}
	if n, err := e.svc.Get(context.Background(), e.ids["alice"], doc.ID); err != nil || n.Name != "Новый.docx" {
		t.Fatalf("renamed to %q, %v", n.Name, err)
	}
	code(t, e.call(t, e.key, "POST", path, tok, "", "X-WOPI-Override", "DELETE"), http.StatusNotImplemented)
}

// OnlyOffice builds its URLs from the address it was asked at: the server
// reaches it as http://onlyoffice, the browser as localhost:9981.
func TestDiscoveryEditorURL(t *testing.T) {
	const doc = `<wopi-discovery><net-zone name="external-http"><app name="Word">` +
		`<action name="view" ext="doc" urlsrc="http://onlyoffice/hosting/wopi/word/view?&lt;ui=UI_LLCC&amp;&gt;&amp;"/>` +
		`<action name="edit" ext="docx" urlsrc="http://onlyoffice/hosting/wopi/word/edit?&lt;ui=UI_LLCC&amp;&gt;&amp;"/>` +
		`</app></net-zone></wopi-discovery>`
	for _, tc := range []struct{ editorURL, origin, docx string }{
		{"", "http://onlyoffice", "http://onlyoffice/hosting/wopi/word/edit?&"},
		{"http://localhost:9981", "http://localhost:9981", "http://localhost:9981/hosting/wopi/word/edit?&"},
	} {
		e, err := parseDiscovery(strings.NewReader(doc), tc.editorURL)
		if err != nil {
			t.Fatal(err)
		}
		if e.Origin != tc.origin || e.Actions["docx"] != (Action{URL: tc.docx, Edit: true}) ||
			e.Actions["doc"].URL != tc.origin+"/hosting/wopi/word/view?&" || e.ProofKey != nil {
			t.Errorf("editor URL %q: %+v", tc.editorURL, e)
		}
	}
}

func TestDecodeUTF7(t *testing.T) {
	for in, want := range map[string]string{
		"Hi Mom -+Jjo--!": "Hi Mom -☺-!",
		"+ZeVnLIqe-":      "日本語",
		"A+ImIDkQ.":       "A≢Α.",
		"1 +- 1":          "1 + 1",
		"plain.docx":      "plain.docx",
	} {
		got, err := decodeUTF7(in)
		if err != nil || got != want {
			t.Errorf("decodeUTF7(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"+", "+!", "ё"} {
		if _, err := decodeUTF7(bad); err == nil {
			t.Errorf("decodeUTF7(%q) accepted", bad)
		}
	}
}

func TestCreateDocument(t *testing.T) {
	e := newEnv(t)
	create := func(user, dir, body string) (*httptest.ResponseRecorder, map[string]string) {
		req := httptest.NewRequest("POST", "/api/v1/nodes/"+dir+"/documents", strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "session", Value: e.session[user]})
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, req)
		var out map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}
	for _, want := range []string{"Новый документ.docx", "Новый документ (2).docx"} {
		rec, out := create("alice", "root", `{"name":"Новый документ.docx"}`)
		code(t, rec, http.StatusCreated)
		if out["name"] != want {
			t.Fatalf("created %v, want %s", out, want)
		}
		if n, err := e.svc.Get(context.Background(), e.ids["alice"], out["id"]); err != nil || n.Size != 0 || n.ParentID != e.root["alice"].ID {
			t.Fatalf("node %+v, %v", n, err)
		}
	}
	// What the editor only shows, or doesn't know, isn't made.
	if rec, _ := create("alice", "root", `{"name":"a.pdf"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("pdf: %d", rec.Code)
	}
	if rec, _ := create("alice", "root", `{"name":"a.exe"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("exe: %d", rec.Code)
	}
	if rec, _ := create("bob", e.root["alice"].ID, `{"name":"a.docx"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("someone else's folder: %d", rec.Code)
	}
}
