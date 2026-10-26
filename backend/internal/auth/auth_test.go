package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/db"
)

func testDatabaseURL() string {
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
}

// testPool returns a pool over an emptied test database.
func testPool(t *testing.T) *pgxpool.Pool {
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
	if _, err := pool.Exec(ctx, `TRUNCATE users, auth_providers CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func insertUser(t *testing.T, pool *pgxpool.Pool, username string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO users (username, display_name) VALUES ($1::text, $1::text) RETURNING id`, username).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func TestSessionLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := NewSessions(pool, 24*time.Hour)
	uid := insertUser(t, pool, "alice")

	raw, expires, err := s.Create(ctx, uid, netip.MustParseAddr("192.0.2.1"), strings.Repeat("x", 2000))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if d := time.Until(expires); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("expires in %s", d)
	}
	u, renewed, err := s.Lookup(ctx, raw)
	if err != nil || u.ID != uid || u.Username != "alice" {
		t.Fatalf("lookup: %+v, %v", u, err)
	}
	if !renewed.IsZero() {
		t.Fatal("a fresh session was renewed")
	}

	// The token is stored only hashed, and the user agent capped.
	var stored int
	var uaLen int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE id_hash = convert_to($1, 'UTF8')), max(length(user_agent)) FROM sessions`, raw).Scan(&stored, &uaLen); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if stored != 0 || uaLen != maxUserAgent {
		t.Fatalf("raw token stored %d times, user agent %d bytes", stored, uaLen)
	}

	// An idle session slides forward once renewEvery has passed.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET last_seen_at = now() - interval '2 hours', expires_at = now() + interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if _, renewed, err = s.Lookup(ctx, raw); err != nil || time.Until(renewed) < 23*time.Hour {
		t.Fatalf("renewal: %s, %v", renewed, err)
	}

	if err := s.Delete(ctx, raw); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := s.Lookup(ctx, raw); !errors.Is(err, ErrNoSession) {
		t.Fatalf("lookup after delete: %v", err)
	}
}

func TestSessionRejected(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := NewSessions(pool, 24*time.Hour)
	uid := insertUser(t, pool, "alice")

	expired, _, _ := s.Create(ctx, uid, netip.Addr{}, "")
	if _, err := pool.Exec(ctx, `UPDATE sessions SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	live, _, _ := s.Create(ctx, uid, netip.Addr{}, "")
	other, _, _ := s.Create(ctx, insertUser(t, pool, "bob"), netip.Addr{}, "")

	cases := map[string]string{"expired": expired, "unknown": "not-a-token"}
	for what, raw := range cases {
		if _, _, err := s.Lookup(ctx, raw); !errors.Is(err, ErrNoSession) {
			t.Errorf("%s: %v", what, err)
		}
	}
	// Disabling a user cuts off their live sessions at once.
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Lookup(ctx, live); !errors.Is(err, ErrNoSession) {
		t.Errorf("disabled user: %v", err)
	}
	if err := s.DeleteForUser(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Lookup(ctx, other); err != nil {
		t.Errorf("another user's session was affected: %v", err)
	}
}

func TestHandlers(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	h := &Handlers{Sessions: NewSessions(pool, 24*time.Hour), Cookies: Cookies{Secure: true}}
	mux := http.NewServeMux()
	h.Register(mux)
	uid := insertUser(t, pool, "alice")
	raw, _, err := h.Sessions.Create(ctx, uid, netip.Addr{}, "")
	if err != nil {
		t.Fatal(err)
	}

	call := func(method, path, cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: hostCookieName, Value: cookie})
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := call("GET", "/api/v1/auth/me", ""); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"unauthorized"`) {
		t.Fatalf("me without cookie: %d %s", rec.Code, rec.Body)
	}
	rec := call("GET", "/api/v1/auth/me", raw)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"username":"alice"`) {
		t.Fatalf("me: %d %s", rec.Code, rec.Body)
	}

	rec = call("POST", "/api/v1/auth/logout", raw)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}
	if c := rec.Result().Cookies(); len(c) != 1 || c[0].MaxAge >= 0 {
		t.Fatalf("logout cookie: %+v", c)
	}
	// The dead cookie gets a 401 and is cleared.
	rec = call("GET", "/api/v1/auth/me", raw)
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("me after logout: %d, cookies %v", rec.Code, rec.Result().Cookies())
	}
}

func TestCookieFlags(t *testing.T) {
	for _, secure := range []bool{true, false} {
		rec := httptest.NewRecorder()
		Cookies{Secure: secure}.Set(rec, "tok", time.Now().Add(time.Hour))
		c := rec.Result().Cookies()[0]
		wantName := plainCookieName
		if secure {
			wantName = hostCookieName
		}
		if c.Name != wantName || !c.HttpOnly || c.Secure != secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
			t.Errorf("secure=%v: %+v", secure, c)
		}
	}
}
