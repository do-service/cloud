package nodes

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// public serves one request to the public side, with the link's cookie if
// one was set.
func (e env) public(t *testing.T, method, path, body string, cookie *http.Cookie, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func TestPublicLinks(t *testing.T) {
	e := newEnv(t, nil)
	docs := e.mkdir(t, e.root.ID, "Docs")
	e.file(t, docs.ID, "a.txt", "hello")
	e.file(t, docs.ID, "page.html", "<script>x</script>")

	rec := e.send(t, "POST", "/api/v1/nodes/"+docs.ID+"/links", `{"mode":"view","password":"pw"}`)
	link := decode[linkJSON](t, rec)
	if rec.Code != http.StatusCreated || link.Token == nil || !link.HasPassword {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	base := "/s/" + *link.Token + "/api"

	info := decode[linkInfoJSON](t, e.public(t, "GET", base+"/info", "", nil, nil))
	if info.Name != "Docs" || info.Mode != "view" || !info.NeedsPassword || info.Unlocked || info.RootID != docs.ID {
		t.Fatalf("info: %+v", info)
	}
	if rec := e.public(t, "GET", base+"/nodes/root/children", "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("locked: %d", rec.Code)
	}
	if rec := e.public(t, "POST", base+"/unlock", `{"password":"nope"}`, nil, nil); rec.Code != http.StatusUnauthorized || errorCode(rec) != "invalid_credentials" {
		t.Fatalf("wrong password: %d %s", rec.Code, rec.Body)
	}
	rec = e.public(t, "POST", base+"/unlock", `{"password":"pw"}`, nil, nil)
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == linkCookie {
			cookie = c
		}
	}
	if rec.Code != http.StatusNoContent || cookie == nil || cookie.Path != "/s/"+*link.Token+"/" || !cookie.HttpOnly {
		t.Fatalf("unlock: %d %+v", rec.Code, cookie)
	}

	rec = e.public(t, "GET", base+"/nodes/root/children", "", cookie, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"a.txt"`) || !strings.Contains(rec.Body.String(), `"permissions":["read"]`) {
		t.Fatalf("children: %d %s", rec.Code, rec.Body)
	}
	page := decode[struct{ Items []nodeJSON }](t, rec)
	var txt, html string
	for _, it := range page.Items {
		if it.Name == "a.txt" {
			txt = it.ID
		} else {
			html = it.ID
		}
	}
	// A view link shows safe types in the browser, and nothing else.
	rec = e.public(t, "GET", base+"/nodes/"+txt+"/content", "", cookie, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" || rec.Header().Get("Content-Disposition") != `inline; filename="a.txt"` {
		t.Fatalf("view text: %d %v", rec.Code, rec.Header())
	}
	if rec := e.public(t, "GET", base+"/nodes/"+html+"/content", "", cookie, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("view html: %d", rec.Code)
	}
	if rec := e.public(t, "GET", base+"/nodes/root/archive", "", cookie, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("zip through a view link: %d", rec.Code)
	}
	if rec := e.public(t, "GET", base+"/nodes/"+e.root.ID+"/children", "", cookie, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("outside the link: %d", rec.Code)
	}

	// Download mode, no password: attachments and ZIP.
	if rec := e.send(t, "PATCH", "/api/v1/links/"+link.ID, `{"mode":"download","password":""}`); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	rec = e.public(t, "GET", base+"/nodes/"+html+"/content", "", nil, nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") || rec.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("download html: %d %v", rec.Code, rec.Header())
	}
	if rec := e.public(t, "GET", base+"/nodes/root/archive", "", nil, nil); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("zip: %d", rec.Code)
	}

	// Upload mode: tus through the link's own mount, nothing to list.
	rec = e.send(t, "POST", "/api/v1/nodes/"+docs.ID+"/links", `{"mode":"upload"}`)
	drop := decode[linkJSON](t, rec)
	dropBase := "/s/" + *drop.Token + "/api"
	if rec := e.public(t, "GET", dropBase+"/nodes/root/children", "", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("list through an upload link: %d", rec.Code)
	}
	tus := map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "2", "Upload-Metadata": "filename " + base64.StdEncoding.EncodeToString([]byte("a.txt"))}
	rec = e.public(t, "POST", dropBase+"/uploads/", "", nil, tus)
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusCreated || !strings.HasPrefix(loc, dropBase+"/uploads/") {
		t.Fatalf("create upload: %d %q %s", rec.Code, loc, rec.Body)
	}
	rec = e.public(t, "PATCH", loc, "hi", nil, map[string]string{"Tus-Resumable": "1.0.0", "Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"})
	if rec.Code != http.StatusNoContent || rec.Header().Get("Upload-Node-Id") == "" {
		t.Fatalf("finish upload: %d %s", rec.Code, rec.Body)
	}
	// Kept beside the existing a.txt: a link never replaces.
	listing := e.send(t, "GET", "/api/v1/nodes/"+docs.ID+"/children", "").Body.String()
	if !strings.Contains(listing, `"a (2).txt"`) {
		t.Fatalf("dropped file: %s", listing)
	}
	if rec := e.public(t, "POST", base+"/uploads/", "", nil, tus); rec.Code != http.StatusForbidden {
		t.Fatalf("upload through a download link: %d", rec.Code)
	}

	// Revoked: the token is unknown.
	if rec := e.send(t, "DELETE", "/api/v1/links/"+link.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := e.public(t, "GET", base+"/info", "", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("after delete: %d", rec.Code)
	}
}

func TestLinkPasswordAttemptsLimited(t *testing.T) {
	e := newEnv(t, nil)
	link := decode[linkJSON](t, e.send(t, "POST", "/api/v1/nodes/"+e.mkdir(t, e.root.ID, "D").ID+"/links", `{"mode":"view","password":"pw"}`))
	unlock := "/s/" + *link.Token + "/api/unlock"
	for i := range 3 {
		if rec := e.public(t, "POST", unlock, `{"password":"nope"}`, nil, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	// Out of tries: refused before checking, even the right password.
	if rec := e.public(t, "POST", unlock, `{"password":"pw"}`, nil, nil); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("after limit: %d", rec.Code)
	}
}
