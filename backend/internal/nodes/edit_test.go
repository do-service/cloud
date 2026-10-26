package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/do-service/cloud/backend/internal/fs"
)

// send serves one request with a JSON body as alice.
func (e env) send(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: e.session})
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

func errorCode(rec *httptest.ResponseRecorder) string {
	var body struct{ Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Code
}

func TestNodeEdits(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.send(t, "POST", "/api/v1/nodes/root/children", `{"name":"Docs"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mkdir: %d %s", rec.Code, rec.Body)
	}
	docs := decode[nodeJSON](t, rec)
	sub := decode[nodeJSON](t, e.send(t, "POST", "/api/v1/nodes/"+docs.ID+"/children", `{"name":"Sub"}`))
	file := e.file(t, e.root.ID, "a.txt", "a")

	rec = e.send(t, "GET", "/api/v1/nodes/"+sub.ID, "")
	got := decode[nodeWithPath](t, rec)
	if rec.Code != http.StatusOK || got.Name != "Sub" || fmt.Sprint(got.Path) != fmt.Sprintf("[{%s } {%s Docs} {%s Sub}]", e.root.ID, docs.ID, sub.ID) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	if got := decode[nodeWithPath](t, e.send(t, "GET", "/api/v1/nodes/root", "")); got.ID != e.root.ID || len(got.Path) != 1 {
		t.Fatalf("get root: %+v", got)
	}

	rec = e.send(t, "PATCH", "/api/v1/nodes/"+sub.ID, `{"name":"Подпапка"}`)
	if rec.Code != http.StatusOK || decode[nodeJSON](t, rec).Name != "Подпапка" {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}
	rec = e.send(t, "PATCH", "/api/v1/nodes/"+file.ID, `{"parentId":"`+docs.ID+`","name":"b.txt"}`)
	if rec.Code != http.StatusOK || decode[nodeJSON](t, rec).Name != "b.txt" {
		t.Fatalf("move and rename: %d %s", rec.Code, rec.Body)
	}
	if rec := e.send(t, "PATCH", "/api/v1/nodes/"+file.ID, `{"parentId":"root"}`); rec.Code != http.StatusOK {
		t.Fatalf("move to root alias: %d %s", rec.Code, rec.Body)
	}

	for _, tc := range []struct {
		name, method, path, body string
		code                     int
		err                      string
	}{
		{"mkdir conflict, other case", "POST", "/api/v1/nodes/root/children", `{"name":"docs"}`, 409, "name_conflict"},
		{"mkdir bad name", "POST", "/api/v1/nodes/root/children", `{"name":"a/b"}`, 400, "invalid_name"},
		{"mkdir no name", "POST", "/api/v1/nodes/root/children", `{}`, 400, "invalid_name"},
		{"mkdir in a file", "POST", "/api/v1/nodes/" + file.ID + "/children", `{"name":"x"}`, 400, "not_a_directory"},
		{"mkdir bad json", "POST", "/api/v1/nodes/root/children", `{`, 400, "invalid_request"},
		{"mkdir bad parent id", "POST", "/api/v1/nodes/nope/children", `{"name":"x"}`, 404, "not_found"},
		{"move into own subfolder", "PATCH", "/api/v1/nodes/" + docs.ID, `{"parentId":"` + sub.ID + `"}`, 409, "move_into_itself"},
		{"move into itself", "PATCH", "/api/v1/nodes/" + docs.ID, `{"parentId":"` + docs.ID + `"}`, 409, "move_into_itself"},
		{"rename root", "PATCH", "/api/v1/nodes/root", `{"name":"x"}`, 400, "root_immutable"},
		{"empty patch", "PATCH", "/api/v1/nodes/" + docs.ID, `{}`, 400, "invalid_request"},
		{"move into a file", "PATCH", "/api/v1/nodes/" + docs.ID, `{"parentId":"` + file.ID + `"}`, 400, "not_a_directory"},
		{"bad id", "PATCH", "/api/v1/nodes/nope", `{"name":"x"}`, 404, "not_found"},
		{"bad parent id", "PATCH", "/api/v1/nodes/" + docs.ID, `{"parentId":"nope"}`, 404, "not_found"},
		{"get bad id", "GET", "/api/v1/nodes/nope", "", 404, "not_found"},
		{"oversized body", "PATCH", "/api/v1/nodes/" + docs.ID, `{"name":"` + strings.Repeat("x", maxNodeBody) + `"}`, 413, "request_body_too_large"},
	} {
		rec := e.send(t, tc.method, tc.path, tc.body)
		if rec.Code != tc.code || errorCode(rec) != tc.err {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
	}

	var bob string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE nodes SET owner_id = $1 WHERE id = $2`, bob, sub.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/nodes/" + sub.ID} {
		if rec := e.send(t, "PATCH", path, `{"name":"mine"}`); rec.Code != http.StatusNotFound {
			t.Errorf("foreign patch: %d", rec.Code)
		}
		if rec := e.send(t, "GET", path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("foreign get: %d", rec.Code)
		}
	}
}

func TestBatch(t *testing.T) {
	e := newEnv(t, nil)
	target := e.mkdir(t, e.root.ID, "Target")
	e.file(t, target.ID, "b.txt", "old")
	a, b, c := e.file(t, e.root.ID, "a.txt", "a"), e.file(t, e.root.ID, "b.txt", "b"), e.file(t, e.root.ID, "c.txt", "c")
	missing := "00000000-0000-0000-0000-000000000000"

	op := func(id, extra string) string {
		return `{"op":"move","id":"` + id + `","parentId":"` + target.ID + `"` + extra + `}`
	}
	rec := e.send(t, "POST", "/api/v1/nodes/batch", `{"ops":[`+op(a.ID, "")+`,`+op(b.ID, "")+`,`+op(c.ID, `,"name":"C.txt"`)+`,`+op(missing, "")+`]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body)
	}
	results := decode[struct{ Results []batchResult }](t, rec).Results
	var got []string
	for _, r := range results {
		switch {
		case r.Error != nil:
			got = append(got, r.ID[:4]+" "+r.Error.Code)
		case r.Node != nil:
			got = append(got, r.ID[:4]+" "+r.Node.Name)
		}
	}
	want := []string{a.ID[:4] + " a.txt", b.ID[:4] + " name_conflict", c.ID[:4] + " C.txt", "0000 not_found"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("results %v, want %v", got, want)
	}
	_, page := e.get(t, "/api/v1/nodes/"+target.ID+"/children", true)
	var names []string
	for _, it := range page.Items {
		names = append(names, it.Name)
	}
	if fmt.Sprint(names) != "[a.txt b.txt C.txt]" {
		t.Fatalf("target holds %v", names)
	}

	tooMany := strings.Repeat(op(a.ID, "")+",", MaxBatchOps) + op(a.ID, "")
	for _, tc := range []struct {
		name, body string
		code       int
	}{
		{"no ops", `{"ops":[]}`, 400},
		{"unknown op", `{"ops":[{"op":"rename","id":"` + a.ID + `","parentId":"` + target.ID + `"}]}`, 400},
		{"move without parent", `{"ops":[{"op":"move","id":"` + a.ID + `"}]}`, 400},
		{"too many", `{"ops":[` + tooMany + `]}`, 400},
		{"bad json", `{"ops":`, 400},
		{"oversized", `{"ops":[` + strings.Repeat(" ", maxBatchBody) + `]}`, 413},
	} {
		if rec := e.send(t, "POST", "/api/v1/nodes/batch", tc.body); rec.Code != tc.code {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
	}
	// A rejected batch changed nothing: a.txt is still in Target.
	if n, err := e.svc.Get(context.Background(), e.user, a.ID); err != nil || n.ParentID != target.ID {
		t.Fatalf("after rejected batches: %+v, %v", n, err)
	}
}

// A file locked by a WebDAV client or an editor can't be changed from the
// web UI either.
func TestLockedFileRefused(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	file, err := e.svc.CreateFile(ctx, e.user, e.root.ID, "a.txt", strings.NewReader("a"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Lock(ctx, e.user, file.ID, fs.LockRequest{Kind: fs.LockWebDAV}); err != nil {
		t.Fatal(err)
	}
	rec := e.send(t, "PATCH", "/api/v1/nodes/"+file.ID, `{"name":"b.txt"}`)
	if rec.Code != http.StatusLocked || errorCode(rec) != "locked" {
		t.Fatalf("rename locked: %d %s", rec.Code, rec.Body)
	}
}

func TestQuotaAPI(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.svc.CreateFile(context.Background(), e.user, e.root.ID, "a.txt", strings.NewReader("abc"), 3); err != nil {
		t.Fatal(err)
	}
	rec := e.send(t, "GET", "/api/v1/me/quota", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"usedBytes":3,"quotaBytes":null}` {
		t.Fatalf("quota: %d %s", rec.Code, rec.Body)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET quota_bytes = 10 WHERE id = $1`, e.user); err != nil {
		t.Fatal(err)
	}
	if rec := e.send(t, "GET", "/api/v1/me/quota", ""); !strings.Contains(rec.Body.String(), `"quotaBytes":10`) {
		t.Fatalf("quota: %s", rec.Body)
	}
}
