package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/do-service/cloud/backend/internal/httpx"
)

const indexHTML = "<!doctype html><title>cloud</title>"

func builtFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte(indexHTML)},
		"favicon.svg":          {Data: []byte("<svg/>")},
		"assets/app-abc123.js": {Data: []byte("console.log(1)")},
		"assets/nested/x.css":  {Data: []byte("body{}")},
		"icons/file.svg":       {Data: []byte("<svg/>")},
	}
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body httpx.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Code
}

func TestHandler_ServesFilesAndFallsBack(t *testing.T) {
	h := New(builtFS())
	cases := []struct {
		name, method, path string
		wantStatus         int
		wantBody           string
		wantCache          string
	}{
		{"root", http.MethodGet, "/", http.StatusOK, indexHTML, "no-cache"},
		{"index explicitly", http.MethodGet, "/index.html", http.StatusOK, indexHTML, "no-cache"},
		{"static file", http.MethodGet, "/favicon.svg", http.StatusOK, "<svg/>", ""},
		{"hashed asset", http.MethodGet, "/assets/app-abc123.js", http.StatusOK, "console.log(1)", "public, max-age=31536000, immutable"},
		{"spa route", http.MethodGet, "/files/Documents/report.pdf", http.StatusOK, indexHTML, "no-cache"},
		{"directory falls back", http.MethodGet, "/icons", http.StatusOK, indexHTML, "no-cache"},
		{"traversal stays inside", http.MethodGet, "/../../etc/passwd", http.StatusOK, indexHTML, "no-cache"},
		{"head", http.MethodHead, "/settings", http.StatusOK, "", "no-cache"},
		{"prefix lookalike is spa", http.MethodGet, "/apiary", http.StatusOK, indexHTML, "no-cache"},
		// A public link's page is the SPA's; only its /api is reserved.
		{"public link page", http.MethodGet, "/s/token", http.StatusOK, indexHTML, "no-cache"},
		{"public link page, deeper", http.MethodGet, "/s/token/apiary", http.StatusOK, indexHTML, "no-cache"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body)
			}
			if rec.Body.String() != tc.wantBody {
				t.Fatalf("body = %q, want %q", rec.Body, tc.wantBody)
			}
			if got := rec.Header().Get("Cache-Control"); got != tc.wantCache {
				t.Fatalf("Cache-Control = %q, want %q", got, tc.wantCache)
			}
		})
	}
}

func TestHandler_ReservedPrefixesDoNotFallBack(t *testing.T) {
	h := New(builtFS())
	for _, p := range []string{"/api", "/api/v2/nope", "/dav/", "/dav/files/x", "/wopi/files/1", "/s/token/api", "/s/token/api/info", "/healthz", "/readyz", "/readyz/x"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, "PROPFIND"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, p, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s: status = %d, want 404", method, p, rec.Code)
			}
			if code := errorCode(t, rec); code != httpx.CodeNotFound {
				t.Fatalf("%s %s: code = %q", method, p, code)
			}
		}
	}
}

func TestHandler_MissingAssetIs404(t *testing.T) {
	rec := httptest.NewRecorder()
	New(builtFS()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app-old.js", nil))
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != httpx.CodeNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandler_RejectsWriteMethods(t *testing.T) {
	rec := httptest.NewRecorder()
	New(builtFS()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/files", nil))
	if rec.Code != http.StatusMethodNotAllowed || errorCode(t, rec) != httpx.CodeMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q", got)
	}
}

func TestHandler_FrontendNotBuilt(t *testing.T) {
	h := New(fstest.MapFS{".gitkeep": {Data: nil}})
	for _, p := range []string{"/", "/files/x"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", p, rec.Code)
		}
		if code := errorCode(t, rec); code != httpx.CodeFrontendNotBuilt {
			t.Fatalf("%s: code = %q", p, code)
		}
	}
}

// The real embedded dist must at least compile and answer; in a
// backend-only checkout that's the frontend_not_built 404.
func TestHandler_Embedded(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}
