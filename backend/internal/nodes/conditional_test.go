package nodes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/do-service/cloud/backend/internal/access"
)

// ifMatchSend serves one request as alice with If-Match set, unless "".
func (e env) ifMatchSend(t *testing.T, method, path, body, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: e.session})
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func TestIfMatch(t *testing.T) {
	e := newEnv(t, nil)
	docs := e.mkdir(t, e.root.ID, "Docs")
	f := e.file(t, docs.ID, "a.txt", "a")
	quoted := `"` + f.ETag + `"`

	rec := e.ifMatchSend(t, "PATCH", "/api/v1/nodes/"+f.ID, `{"name":"b.txt"}`, `"stale"`)
	if rec.Code != http.StatusPreconditionFailed || errorCode(rec) != "precondition_failed" {
		t.Fatalf("stale rename: %d %s", rec.Code, rec.Body)
	}
	rec = e.ifMatchSend(t, "PATCH", "/api/v1/nodes/"+f.ID, `{"name":"b.txt"}`, `"stale", `+quoted)
	if rec.Code != http.StatusOK || decode[nodeJSON](t, rec).Name != "b.txt" {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}
	if rec := e.ifMatchSend(t, "PATCH", "/api/v1/nodes/"+f.ID, `{"parentId":"root"}`, "W/"+quoted); rec.Code != http.StatusOK {
		t.Fatalf("move, weak tag: %d %s", rec.Code, rec.Body)
	}
	if rec := e.ifMatchSend(t, "PATCH", "/api/v1/nodes/"+f.ID, `{"name":"c.txt"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("rename without If-Match: %d %s", rec.Code, rec.Body)
	}

	// A folder's ETag moved on when the file left it.
	if rec := e.ifMatchSend(t, "DELETE", "/api/v1/nodes/"+docs.ID, "", `"`+docs.ETag+`"`); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale delete: %d %s", rec.Code, rec.Body)
	}
	cur := decode[nodeWithPath](t, e.send(t, "GET", "/api/v1/nodes/"+docs.ID, ""))
	if rec := e.ifMatchSend(t, "DELETE", "/api/v1/nodes/"+docs.ID, "", `"`+cur.ETag+`"`); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := e.ifMatchSend(t, "DELETE", "/api/v1/nodes/"+f.ID, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete without If-Match: %d %s", rec.Code, rec.Body)
	}
}

// Nodes carry their parent and, files, the content's SHA-256; the top of
// what a recipient sees has no parent.
func TestNodeParentAndHash(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "Docs")
	f := e.file(t, docs.ID, "a.txt", "abc")
	sum := sha256.Sum256([]byte("abc"))

	got := decode[nodeWithPath](t, e.send(t, "GET", "/api/v1/nodes/"+f.ID, ""))
	if got.ParentID == nil || *got.ParentID != docs.ID || got.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("file: %+v", got)
	}
	if got := decode[nodeWithPath](t, e.send(t, "GET", "/api/v1/nodes/root", "")); got.ParentID != nil || got.SHA256 != "" {
		t.Fatalf("root: %+v", got)
	}
	if rec := e.send(t, "GET", "/api/v1/nodes/root/children", ""); !strings.Contains(rec.Body.String(), `"parentId":"`+e.root.ID+`"`) || strings.Contains(rec.Body.String(), `"sha256"`) {
		t.Fatalf("listing: %s", rec.Body)
	}

	var bob string
	if err := e.pool.QueryRow(ctx, `INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.EnsureRoot(ctx, bob); err != nil {
		t.Fatal(err)
	}
	session, _, err := e.sessions.Create(ctx, bob, netip.Addr{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO shares (node_id, grantee_type, grantee_id, perms) VALUES ($1, 'user', $2, $3)`,
		docs.ID, bob, int16(access.Read)); err != nil {
		t.Fatal(err)
	}
	if got := decode[nodeWithPath](t, e.as(t, session, "GET", "/api/v1/nodes/"+docs.ID, "")); got.ParentID != nil {
		t.Fatalf("shared top: %+v", got)
	}
	if got := decode[nodeWithPath](t, e.as(t, session, "GET", "/api/v1/nodes/"+f.ID, "")); got.ParentID == nil || *got.ParentID != docs.ID {
		t.Fatalf("inside the share: %+v", got)
	}
	if rec := e.as(t, session, "GET", "/api/v1/shared-with-me", ""); !strings.Contains(rec.Body.String(), `"parentId":null`) {
		t.Fatalf("shared with me: %s", rec.Body)
	}
}
