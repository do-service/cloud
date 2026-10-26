package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/ratelimit"
)

func TestCreateLocalValidates(t *testing.T) {
	pool := testPool(t)
	users := NewUsers(pool)
	ctx := context.Background()
	if _, err := users.CreateLocal(ctx, "alice", "", "password1", false); err != nil {
		t.Fatalf("create: %v", err)
	}
	cases := []struct {
		username, password string
		want               error
	}{
		{"ALICE", "password1", ErrUsernameTaken}, // citext: case-insensitive
		{"bob/../x", "password1", ErrInvalidUsername},
		{"", "password1", ErrInvalidUsername},
		{".hidden", "password1", ErrInvalidUsername},
		{"bob", "short", ErrWeakPassword},
		{"bob", strings.Repeat("x", maxPassword+1), ErrWeakPassword},
	}
	for _, tc := range cases {
		if _, err := users.CreateLocal(ctx, tc.username, "", tc.password, false); !errors.Is(err, tc.want) {
			t.Errorf("%q: err %v, want %v", tc.username, err, tc.want)
		}
	}
}

func TestAuthenticate(t *testing.T) {
	pool := testPool(t)
	users := NewUsers(pool)
	ctx := context.Background()
	alice, err := users.CreateLocal(ctx, "alice", "Алиса", "password1", true)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := users.Authenticate(ctx, "Alice", "password1"); err != nil || u.ID != alice.ID || !u.IsAdmin || u.DisplayName != "Алиса" {
		t.Fatalf("login: %+v, %v", u, err)
	}
	if has, err := users.HasAdmin(ctx); err != nil || !has {
		t.Fatalf("HasAdmin: %v, %v", has, err)
	}
	// An OIDC-only account has no password to match.
	if _, err := pool.Exec(ctx, `INSERT INTO users (username) VALUES ('oidc')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (username, password_hash, status) SELECT 'gone', password_hash, 'disabled' FROM users WHERE username = 'alice'`); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"alice", "wrong"}, {"nobody", "password1"}, {"oidc", "password1"}, {"gone", "password1"}} {
		if _, err := users.Authenticate(ctx, c[0], c[1]); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s: err %v", c[0], err)
		}
	}
}

type loginEnv struct {
	mux *http.ServeMux
	h   *Handlers
}

func newLoginEnv(t *testing.T, perUser *ratelimit.Limiter) loginEnv {
	t.Helper()
	pool := testPool(t)
	h := &Handlers{
		Sessions:     NewSessions(pool, 24*time.Hour),
		Cookies:      Cookies{},
		Users:        NewUsers(pool),
		LoginPerUser: perUser,
	}
	if _, err := h.Users.CreateLocal(context.Background(), "alice", "", "password1", false); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return loginEnv{mux: mux, h: h}
}

func (e loginEnv) login(body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(body))
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func TestLogin(t *testing.T) {
	e := newLoginEnv(t, nil)

	rec := e.login(`{"username":"alice","password":"password1"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"username":"alice"`) {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != plainCookieName || cookies[0].Value == "" {
		t.Fatalf("cookie %+v", cookies)
	}
	first := cookies[0]

	// The cookie works for /me.
	req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	req.AddCookie(first)
	me := httptest.NewRecorder()
	e.mux.ServeHTTP(me, req)
	if me.Code != http.StatusOK {
		t.Fatalf("me: %d", me.Code)
	}

	// Logging in again with that cookie replaces the session.
	second := e.login(`{"username":"alice","password":"password1"}`, first).Result().Cookies()[0]
	if second.Value == first.Value {
		t.Fatal("login reused the token the browser came with")
	}
	if _, _, err := e.h.Sessions.Lookup(context.Background(), first.Value); !errors.Is(err, ErrNoSession) {
		t.Fatalf("previous session still alive: %v", err)
	}

	for body, want := range map[string]int{
		`{"username":"alice","password":"nope"}`:       http.StatusUnauthorized,
		`{"username":"nobody","password":"password1"}`: http.StatusUnauthorized,
		`{"username":"alice"}`:                         http.StatusBadRequest,
		`not json`:                                     http.StatusBadRequest,
	} {
		if rec := e.login(body); rec.Code != want || len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s: %d, cookies %v", body, rec.Code, rec.Result().Cookies())
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newLoginEnv(t, ratelimit.New(time.Hour, 2))
	wrong := `{"username":"alice","password":"nope"}`
	for range 2 {
		if rec := e.login(wrong); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt within burst: %d", rec.Code)
		}
	}
	// Even the right password is refused once the bucket is empty, and the
	// key is the username whatever its case.
	rec := e.login(`{"username":"ALICE","password":"password1"}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), `"too_many_requests"`) {
		t.Fatalf("over limit: %d %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
}

func TestLoginAudited(t *testing.T) {
	e := newLoginEnv(t, nil)
	ctx := context.Background()
	if _, err := e.h.Users.db.Exec(ctx, `TRUNCATE audit_log`); err != nil {
		t.Fatal(err)
	}
	e.h.Audit = audit.New(e.h.Users.db)
	mux := http.NewServeMux()
	e.h.Register(mux)
	e.mux = mux

	e.login(`{"username":"alice","password":"wrong-one"}`)
	ok := e.login(`{"username":"alice","password":"password1"}`)
	req := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	for _, c := range ok.Result().Cookies() {
		req.AddCookie(c)
	}
	e.mux.ServeHTTP(httptest.NewRecorder(), req)

	entries, _, err := e.h.Audit.List(ctx, audit.Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range entries {
		got = append(got, x.Action+":"+x.Actor.Name)
	}
	if strings.Join(got, ",") != "auth.logout:alice,auth.login:alice,auth.login_failed:alice" {
		t.Fatalf("audit = %v", got)
	}
	if entries[2].Actor.ID != "" || strings.Contains(fmt.Sprint(entries[2].Details), "wrong-one") {
		t.Fatalf("failed login entry = %+v", entries[2])
	}
}
