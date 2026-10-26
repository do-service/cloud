package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/do-service/cloud/backend/internal/ratelimit"
)

func TestAppPasswordFormat(t *testing.T) {
	shape := regexp.MustCompile(`^[a-z2-7]{5}(-[a-z2-7]{5}){4}$`)
	seen := map[string]bool{}
	for range 100 {
		p := generateAppPassword()
		if !shape.MatchString(p) || seen[p] {
			t.Fatalf("password %q (repeat %v)", p, seen[p])
		}
		seen[p] = true
	}
	for in, ok := range map[string]bool{
		"abcde-fghij-klmno-pqrst-uvwxy": true,
		"ABCDEFGHIJKLMNOPQRSTUVWXY":     true,
		"abcde-fghij-klmno-pqrst-uvwx":  false, // short
		"abcde-fghij-klmno-pqrst-uvwx1": false, // 1 isn't in the alphabet
		"password1":                     false,
	} {
		if _, got := canonicalAppPassword(in); got != ok {
			t.Errorf("canonical(%q) ok = %v", in, got)
		}
	}
}

func TestAppPasswordLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	apps := NewAppPasswords(pool)
	alice := insertUser(t, pool, "alice")
	bob := insertUser(t, pool, "bob")

	p, password, err := apps.Create(ctx, alice, "  Ноутбук  ")
	if err != nil || p.Name != "Ноутбук" || p.LastUsedAt != nil {
		t.Fatalf("create: %+v, %v", p, err)
	}
	// Typed back without dashes and in capitals, it still works.
	typed := strings.ToUpper(strings.ReplaceAll(password, "-", ""))
	if u, err := apps.Authenticate(ctx, "ALICE", typed); err != nil || u.ID != alice {
		t.Fatalf("authenticate: %+v, %v", u, err)
	}
	list, err := apps.List(ctx, alice)
	if err != nil || len(list) != 1 || list[0].LastUsedAt == nil {
		t.Fatalf("list after use: %+v, %v", list, err)
	}

	// Use is recorded at most hourly, not on every request.
	if _, err := pool.Exec(ctx, `UPDATE app_passwords SET last_used_at = now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Authenticate(ctx, "alice", password); err != nil {
		t.Fatal(err)
	}
	if list, _ = apps.List(ctx, alice); time.Since(*list[0].LastUsedAt) < 9*time.Minute {
		t.Fatalf("last_used_at rewritten within the hour: %s", list[0].LastUsedAt)
	}

	for _, tc := range []struct{ user, password string }{
		{"bob", password},      // someone else's password
		{"alice", "password1"}, // the account password shape
		{"alice", generateAppPassword()},
		{"nobody", password},
	} {
		if _, err := apps.Authenticate(ctx, tc.user, tc.password); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s/%s: %v", tc.user, tc.password, err)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Authenticate(ctx, "alice", password); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled user: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'active' WHERE id = $1`, alice); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{p.ID, "not-a-uuid"} {
		if err := apps.Delete(ctx, bob, id); !errors.Is(err, ErrNoAppPassword) {
			t.Fatalf("delete %q as bob: %v", id, err)
		}
	}
	if err := apps.Delete(ctx, alice, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Authenticate(ctx, "alice", password); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("after revoke: %v", err)
	}
}

func TestAppPasswordLimits(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	apps := NewAppPasswords(pool)
	uid := insertUser(t, pool, "alice")
	for _, name := range []string{"", "   ", strings.Repeat("я", maxAppName+1)} {
		if _, _, err := apps.Create(ctx, uid, name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("name %q: %v", name, err)
		}
	}
	// Concurrent creates can't overshoot the cap.
	var wg sync.WaitGroup
	var mu sync.Mutex
	made := 0
	for range MaxAppPasswords + 10 {
		wg.Go(func() {
			if _, _, err := apps.Create(ctx, uid, "device"); err == nil {
				mu.Lock()
				made++
				mu.Unlock()
			} else if !errors.Is(err, ErrTooManyAppPassword) {
				t.Errorf("create: %v", err)
			}
		})
	}
	wg.Wait()
	if made != MaxAppPasswords {
		t.Fatalf("created %d, cap %d", made, MaxAppPasswords)
	}
}

func TestAppPasswordAPI(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	h := &Handlers{Sessions: NewSessions(pool, 24*time.Hour), Cookies: Cookies{Secure: true}, AppPasswords: NewAppPasswords(pool)}
	mux := http.NewServeMux()
	h.Register(mux)
	uid := insertUser(t, pool, "alice")
	raw, _, err := h.Sessions.Create(ctx, uid, netip.Addr{}, "")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, signedIn bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if signedIn {
			req.AddCookie(&http.Cookie{Name: hostCookieName, Value: raw})
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := call("GET", "/api/v1/me/app-passwords", "", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("list signed out: %d", rec.Code)
	}
	if rec := call("GET", "/api/v1/me/app-passwords", "", true); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty list: %d %s", rec.Code, rec.Body)
	}
	rec := call("POST", "/api/v1/me/app-passwords", `{"name":"rclone"}`, true)
	var created struct {
		ID, Name, Password string
	}
	if rec.Code != http.StatusCreated || rec.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.Password == "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := call("POST", "/api/v1/me/app-passwords", `{"name":""}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name: %d", rec.Code)
	}
	rec = call("GET", "/api/v1/me/app-passwords", "", true)
	if !strings.Contains(rec.Body.String(), `"name":"rclone"`) || strings.Contains(rec.Body.String(), created.Password) || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("list leaks or misses: %s", rec.Body)
	}
	if rec := call("DELETE", "/api/v1/me/app-passwords/"+created.ID, "", true); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := call("DELETE", "/api/v1/me/app-passwords/"+created.ID, "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("delete again: %d", rec.Code)
	}
}

func TestBasicAuth(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	apps := NewAppPasswords(pool)
	uid := insertUser(t, pool, "alice")
	_, password, err := apps.Create(ctx, uid, "dav")
	if err != nil {
		t.Fatal(err)
	}
	failures := ratelimit.New(time.Hour, 3)
	h := BasicAuth(apps, failures)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFromContext(r.Context())
		_, _ = w.Write([]byte(u.Username))
	}))
	call := func(user, pass string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PROPFIND", "/dav/", nil)
		req.RemoteAddr = "192.0.2.7:4242"
		if auth {
			req.SetBasicAuth(user, pass)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := call("", "", false)
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), `Basic realm="Cloud"`) {
		t.Fatalf("no credentials: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	// Successes never count against the limit, however many there are.
	for range 10 {
		if rec := call("alice", password, true); rec.Code != http.StatusOK || rec.Body.String() != "alice" {
			t.Fatalf("app password: %d %s", rec.Code, rec.Body)
		}
	}
	for range 3 {
		if rec := call("alice", "wrong", true); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password: %d", rec.Code)
		}
	}
	// Out of failures: refused before checking, even the right password.
	rec = call("alice", password, true)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("after failures: %d", rec.Code)
	}
}
