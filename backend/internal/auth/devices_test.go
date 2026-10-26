package auth

import (
	"context"
	"crypto/sha256"
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

	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/storage/localfs"
)

func TestDeviceTokenFormat(t *testing.T) {
	shape := regexp.MustCompile(`^cld_[A-Za-z0-9_-]{43}$`)
	seen := map[string]bool{}
	for range 100 {
		tok := generateDeviceToken()
		if !shape.MatchString(tok) || len(tok) != deviceTokenLen || seen[tok] {
			t.Fatalf("token %q (repeat %v)", tok, seen[tok])
		}
		seen[tok] = true
	}
}

func TestDeviceLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	devices := NewDevices(pool)
	alice := insertUser(t, pool, "alice")
	bob := insertUser(t, pool, "bob")

	for _, c := range []struct {
		name, platform, version string
		want                    error
	}{
		{"", "macos", "1.0", ErrInvalidName},
		{strings.Repeat("я", maxAppName+1), "macos", "1.0", ErrInvalidName},
		{"Ноутбук", "android", "1.0", ErrInvalidPlatform},
		{"Ноутбук", "macos", strings.Repeat("1", maxClientVersion+1), ErrInvalidVersion},
	} {
		if _, _, err := devices.Create(ctx, alice, c.name, c.platform, c.version); !errors.Is(err, c.want) {
			t.Errorf("create %q %q: %v, want %v", c.name, c.platform, err, c.want)
		}
	}

	dev, token, err := devices.Create(ctx, alice, "  Ноутбук  ", "macos", "0.1.0")
	if err != nil || dev.Name != "Ноутбук" || dev.Platform != "macos" || dev.ClientVersion != "0.1.0" || dev.LastUsedAt != nil {
		t.Fatalf("create: %+v, %v", dev, err)
	}
	// Only the SHA-256 of the token is stored.
	sum := sha256.Sum256([]byte(token))
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM devices WHERE token_hash = $1 AND position(convert_to($2, 'UTF8') IN token_hash) = 0`, sum[:], token).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored hash: %d, %v", stored, err)
	}

	ip := netip.MustParseAddr("192.0.2.7")
	u, got, err := devices.Lookup(ctx, token, ip)
	if err != nil || u.ID != alice || got.ID != dev.ID {
		t.Fatalf("lookup: %+v %+v, %v", u, got, err)
	}
	list, err := devices.List(ctx, alice)
	if err != nil || len(list) != 1 || list[0].LastUsedAt == nil || list[0].LastIP == nil || *list[0].LastIP != "192.0.2.7" {
		t.Fatalf("list after use: %+v, %v", list, err)
	}

	for what, tok := range map[string]string{
		"unknown":       generateDeviceToken(),
		"no prefix":     strings.TrimPrefix(token, DeviceTokenPrefix),
		"truncated":     token[:len(token)-1],
		"session-alike": "x" + token[1:],
	} {
		if _, _, err := devices.Lookup(ctx, tok, ip); !errors.Is(err, ErrNoDevice) {
			t.Errorf("%s: %v", what, err)
		}
	}

	if _, err := devices.Delete(ctx, bob, dev.ID); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("bob deletes alice's device: %v", err)
	}
	if _, err := devices.Delete(ctx, alice, "not-a-uuid"); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("malformed id: %v", err)
	}

	// Blocking the account cuts its devices off at once.
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, alice); err != nil {
		t.Fatal(err)
	}
	if _, _, err := devices.Lookup(ctx, token, ip); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("disabled user: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'active' WHERE id = $1`, alice); err != nil {
		t.Fatal(err)
	}

	if gone, err := devices.Delete(ctx, alice, dev.ID); err != nil || gone.Name != "Ноутбук" {
		t.Fatalf("delete: %+v, %v", gone, err)
	}
	if _, _, err := devices.Lookup(ctx, token, ip); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("revoked: %v", err)
	}
}

func TestDeviceLastUsedThrottled(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	devices := NewDevices(pool)
	dev, token, err := devices.Create(ctx, insertUser(t, pool, "alice"), "PC", "linux", "")
	if err != nil {
		t.Fatal(err)
	}
	lastIP := func() string {
		var ip string
		if err := pool.QueryRow(ctx, `SELECT coalesce(host(last_ip), '') FROM devices WHERE id = $1`, dev.ID).Scan(&ip); err != nil {
			t.Fatal(err)
		}
		return ip
	}
	if _, _, err := devices.Lookup(ctx, token, netip.MustParseAddr("192.0.2.1")); err != nil || lastIP() != "192.0.2.1" {
		t.Fatalf("first use: %v, ip %q", err, lastIP())
	}
	// Within the hour, a request from elsewhere writes nothing.
	if _, err := pool.Exec(ctx, `UPDATE devices SET last_used_at = now() - interval '59 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := devices.Lookup(ctx, token, netip.MustParseAddr("192.0.2.2")); err != nil || lastIP() != "192.0.2.1" {
		t.Fatalf("within the hour: %v, ip %q", err, lastIP())
	}
	if _, err := pool.Exec(ctx, `UPDATE devices SET last_used_at = now() - interval '61 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := devices.Lookup(ctx, token, netip.MustParseAddr("192.0.2.3")); err != nil || lastIP() != "192.0.2.3" {
		t.Fatalf("after the hour: %v, ip %q", err, lastIP())
	}
	var fresh bool
	if err := pool.QueryRow(ctx, `SELECT last_used_at > now() - interval '1 minute' FROM devices WHERE id = $1`, dev.ID).Scan(&fresh); err != nil || !fresh {
		t.Fatalf("last_used_at not moved: %v", err)
	}
}

func TestDeviceLimit(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	devices := NewDevices(pool)
	uid := insertUser(t, pool, "alice")
	// Concurrent creates can't overshoot the cap.
	var wg sync.WaitGroup
	var mu sync.Mutex
	made := 0
	for range MaxDevices + 10 {
		wg.Go(func() {
			if _, _, err := devices.Create(ctx, uid, "PC", "windows", "1"); err == nil {
				mu.Lock()
				made++
				mu.Unlock()
			} else if !errors.Is(err, ErrTooManyDevices) {
				t.Errorf("create: %v", err)
			}
		})
	}
	wg.Wait()
	if made != MaxDevices {
		t.Fatalf("created %d, cap %d", made, MaxDevices)
	}
}

func TestDeviceMiddleware(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	files := fs.New(pool, store)
	devices := NewDevices(pool)
	h := &Handlers{Sessions: NewSessions(pool, 24*time.Hour), Cookies: Cookies{}, AppPasswords: NewAppPasswords(pool), Devices: devices, Audit: audit.New(pool)}
	mux := http.NewServeMux()
	h.Register(mux)
	signedIn := func(f http.HandlerFunc) http.Handler {
		return Middleware(h.Sessions, devices, h.Cookies)(RequireUser(f))
	}
	// Stand-ins for routes other packages mount behind the same
	// middleware: an admin page, and a write to the user's tree.
	mux.Handle("GET /api/v1/admin/users", signedIn(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	mux.Handle("POST /api/v1/mkdir", signedIn(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFromContext(r.Context())
		root, err := files.Root(r.Context(), u.ID)
		if err == nil {
			_, err = files.CreateDir(r.Context(), u.ID, root.ID, r.URL.Query().Get("name"))
		}
		if err != nil {
			t.Errorf("mkdir: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))

	alice := insertUser(t, pool, "alice")
	bob := insertUser(t, pool, "bob")
	for _, id := range []string{alice, bob} {
		if _, err := files.EnsureRoot(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	dev, token, err := devices.Create(ctx, alice, "Ноутбук", "macos", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := h.Sessions.Create(ctx, alice, netip.Addr{}, "")
	if err != nil {
		t.Fatal(err)
	}
	bobSession, _, err := h.Sessions.Create(ctx, bob, netip.Addr{}, "")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, bearer, cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: plainCookieName, Value: cookie})
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	rec := call("GET", "/api/v1/auth/me", token, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"username":"alice"`) {
		t.Fatalf("me by device: %d %s", rec.Code, rec.Body)
	}
	// The bearer token decides, whatever cookie comes with it; a bad one
	// doesn't fall back to the cookie either.
	if rec := call("GET", "/api/v1/auth/me", token, bobSession); !strings.Contains(rec.Body.String(), `"username":"alice"`) {
		t.Fatalf("bearer with bob's cookie: %d %s", rec.Code, rec.Body)
	}
	if rec := call("GET", "/api/v1/auth/me", generateDeviceToken(), session); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token with a live cookie: %d", rec.Code)
	}

	for _, r := range []struct{ method, path string }{
		{"GET", "/api/v1/admin/users"},
		{"GET", "/api/v1/me/app-passwords"},
		{"POST", "/api/v1/me/app-passwords"},
		{"GET", "/api/v1/me/devices"},
		{"DELETE", "/api/v1/me/devices/" + dev.ID},
	} {
		rec := call(r.method, r.path, token, "")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"device_forbidden"`) {
			t.Errorf("%s %s by device: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
	}

	// The device's writes are journaled as its own; the browser's aren't.
	if rec := call("POST", "/api/v1/mkdir?name=device", token, ""); rec.Code != http.StatusCreated {
		t.Fatalf("mkdir by device: %d", rec.Code)
	}
	if rec := call("POST", "/api/v1/mkdir?name=browser", "", session); rec.Code != http.StatusCreated {
		t.Fatalf("mkdir by session: %d", rec.Code)
	}
	rows, err := pool.Query(ctx, `
		SELECT n.name, coalesce(j.device_id::text, '') FROM sync_journal j JOIN nodes n ON n.id = j.node_id
		WHERE j.owner_id = $1 AND n.name IN ('device', 'browser') ORDER BY j.seq`, alice)
	if err != nil {
		t.Fatal(err)
	}
	var journal []string
	for rows.Next() {
		var name, device string
		if err := rows.Scan(&name, &device); err != nil {
			t.Fatal(err)
		}
		journal = append(journal, name+"="+device)
	}
	if want := []string{"device=" + dev.ID, "browser="}; strings.Join(journal, ",") != strings.Join(want, ",") {
		t.Fatalf("journal = %v, want %v", journal, want)
	}

	// The web settings list and revoke devices; signing out is the
	// device's own call.
	rec = call("GET", "/api/v1/me/devices", "", session)
	var list []Device
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list) != 1 || list[0].ID != dev.ID || strings.Contains(rec.Body.String(), token) {
		t.Fatalf("list by session: %d %s", rec.Code, rec.Body)
	}
	if rec := call("DELETE", "/api/v1/me/devices/current", "", session); rec.Code != http.StatusNotFound {
		t.Fatalf("sign-out without a device: %d", rec.Code)
	}
	if rec := call("DELETE", "/api/v1/me/devices/current", token, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("sign-out: %d %s", rec.Code, rec.Body)
	}
	if rec := call("GET", "/api/v1/auth/me", token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after sign-out: %d", rec.Code)
	}

	other, otherToken, err := devices.Create(ctx, alice, "Рабочий", "windows", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if rec := call("DELETE", "/api/v1/me/devices/"+other.ID, "", bobSession); rec.Code != http.StatusNotFound {
		t.Fatalf("bob revokes alice's device: %d", rec.Code)
	}
	if rec := call("DELETE", "/api/v1/me/devices/"+other.ID, "", session); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := call("GET", "/api/v1/auth/me", otherToken, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after revoke: %d", rec.Code)
	}

	var actions []string
	rows, err = pool.Query(ctx, `SELECT action || ':' || target_id FROM audit_log WHERE action LIKE 'device.%' AND actor_id = $1 ORDER BY id`, alice)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, a)
	}
	if want := []string{audit.DeviceSignOut + ":" + dev.ID, audit.DeviceRevoke + ":" + other.ID}; strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("audit = %v, want %v", actions, want)
	}
}
