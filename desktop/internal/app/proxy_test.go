package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/do-service/cloud/desktop/internal/api"
)

// fakeServer serves /api/v1/me to the holder of token cld_alice, and
// fails the request if anything of the page's reaches it.
func fakeServer(w http.ResponseWriter, r *http.Request) {
	for _, h := range []string{"Origin", "Referer", "Sec-Fetch-Site", "Cookie"} {
		if r.Header.Get(h) != "" {
			http.Error(w, h+" leaked", http.StatusTeapot)
			return
		}
	}
	if r.URL.Path != "/api/v1/me" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer cld_alice" {
		http.Error(w, `{"code":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "x"})
	_, _ = io.WriteString(w, `{"username":"alice"}`)
}

func proxyTo(t *testing.T, token string) http.Handler {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fakeServer))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return NewProxy(func() (Upstream, error) { return Upstream{Server: target, Token: api.StaticToken(token)}, nil })
}

// get asks as the WebView does: its own origin and cookies, which must
// not reach the server.
func get(p http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "wails://localhost/api/v1/me", nil)
	req.Header.Set("Origin", "wails://localhost")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Cookie", "page=1")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func TestProxyAddsTokenHidesCookies(t *testing.T) {
	rec := get(proxyTo(t, "cld_alice"))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"username":"alice"}` {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("Set-Cookie reached the page")
	}
}

func TestProxyRevokedToken(t *testing.T) {
	if rec := get(proxyTo(t, "cld_revoked")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want the server's 401 passed on", rec.Code)
	}
}

func TestProxyNoConnection(t *testing.T) {
	rec := get(NewProxy(func() (Upstream, error) { return Upstream{}, ErrNoUpstream }))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestProxyServerDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	target, _ := url.Parse(srv.URL)
	srv.Close()
	rec := get(NewProxy(func() (Upstream, error) { return Upstream{Server: target, Token: api.StaticToken("t")}, nil }))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
}
