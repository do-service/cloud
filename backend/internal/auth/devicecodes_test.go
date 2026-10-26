package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/ratelimit"
)

func TestCheckRedirectURI(t *testing.T) {
	for raw, ok := range map[string]bool{
		"http://127.0.0.1:53682/cb":        true,
		"http://[::1]:53682/cb":            true,
		"http://127.0.0.1:1024/":           true,
		"http://127.0.0.1:65535/a/b":       true,
		"http://localhost:53682/cb":        false,
		"https://127.0.0.1:53682/cb":       false,
		"http://127.0.0.2:53682/cb":        false,
		"http://127.0.0.1/cb":              false,
		"http://127.0.0.1:53682":           false,
		"http://127.0.0.1:1023/cb":         false,
		"http://127.0.0.1:65536/cb":        false,
		"http://127.0.0.1:053682/cb":       false,
		"http://127.0.0.1:+5368/cb":        false,
		"http://127.0.0.1:53682/cb?x=1":    false,
		"http://127.0.0.1:53682/cb?":       false,
		"http://127.0.0.1:53682/cb#f":      false,
		"http://127.0.0.1:53682/a b":       false,
		"http://127.0.0.1:53682\\@evil/cb": false,
		"http://127.0.0.1:53682@evil/cb":   false,
		"http://127.0.0.1.evil.com:80/cb":  false,
		"http://[::ffff:127.0.0.1]:5368/":  false,
		"":                                 false,
	} {
		if got := checkRedirectURI(raw) == nil; got != ok {
			t.Errorf("checkRedirectURI(%q) ok = %v, want %v", raw, got, ok)
		}
	}
}

func TestCheckChallengeAndVerifier(t *testing.T) {
	verifier := strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	good := base64.RawURLEncoding.EncodeToString(sum[:])
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	if err := checkChallenge(good, "S256"); err != nil {
		t.Fatalf("good challenge: %v", err)
	}
	for what, c := range map[string][2]string{
		"plain":        {good, "plain"},
		"no method":    {good, ""},
		"short":        {good[:42], "S256"},
		"padded":       {good + "=", "S256"},
		"std alphabet": {strings.Repeat("+", 43), "S256"},
		// The last character's low bits fall past the 256th: setting one
		// is another spelling of the same bytes.
		"loose bits": {good[:42] + string(alphabet[strings.IndexByte(alphabet, good[42])^1]), "S256"},
	} {
		if checkChallenge(c[0], c[1]) == nil {
			t.Errorf("%s accepted", what)
		}
	}
	for v, ok := range map[string]bool{
		verifier:                 true,
		strings.Repeat("a", 128): true,
		"aA0-._~" + verifier:     true,
		strings.Repeat("v", 42):  false,
		strings.Repeat("v", 129): false,
		verifier[:42] + "/":      false,
		verifier[:42] + " ":      false,
		verifier[:40] + "éé":     false,
	} {
		if checkVerifier(v) != ok {
			t.Errorf("checkVerifier(%q) != %v", v, ok)
		}
	}
}

type signInEnv struct {
	t       *testing.T
	h       *Handlers
	mux     *http.ServeMux
	alice   string
	session string
}

func newSignInEnv(t *testing.T, perIP *ratelimit.Limiter) *signInEnv {
	pool := testPool(t)
	h := &Handlers{Sessions: NewSessions(pool, 24*time.Hour), Devices: NewDevices(pool), DeviceTokenPerIP: perIP, Audit: audit.New(pool)}
	if _, err := pool.Exec(context.Background(), `TRUNCATE audit_log`); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	e := &signInEnv{t: t, h: h, mux: mux, alice: insertUser(t, pool, "alice")}
	raw, _, err := h.Sessions.Create(context.Background(), e.alice, netip.Addr{}, "")
	if err != nil {
		t.Fatal(err)
	}
	e.session = raw
	return e
}

// do sends body as JSON; cred is a session cookie or "Bearer …".
func (e *signInEnv) do(path string, body any, cred string) *httptest.ResponseRecorder {
	e.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		e.t.Fatal(err)
	}
	req := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	switch {
	case strings.HasPrefix(cred, "Bearer "):
		req.Header.Set("Authorization", cred)
	case cred != "":
		req.AddCookie(&http.Cookie{Name: plainCookieName, Value: cred})
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

const testRedirect = "http://127.0.0.1:53682/cb"

// pkce is a client's verifier and its challenge.
func pkce(seed string) (verifier, challenge string) {
	verifier = strings.Repeat(seed, 43)[:43]
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func grantBody(challenge string) map[string]any {
	return map[string]any{
		"redirectUri": testRedirect, "codeChallenge": challenge, "codeChallengeMethod": "S256",
		"state": "st&ate=1", "name": "Ноутбук Василия", "platform": "macos", "clientVersion": "0.1.0",
	}
}

// authorize consents as Alice and returns the code from the redirect.
func (e *signInEnv) authorize(challenge string) string {
	e.t.Helper()
	rec := e.do("/api/v1/devices/authorize", grantBody(challenge), e.session)
	var resp redirectResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		e.t.Fatalf("authorize: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		e.t.Errorf("authorize Cache-Control %q", rec.Header().Get("Cache-Control"))
	}
	u, err := url.Parse(resp.Redirect)
	if err != nil || !strings.HasPrefix(resp.Redirect, testRedirect+"?") || u.Query().Get("state") != "st&ate=1" || len(u.Query()) != 2 {
		e.t.Fatalf("redirect %q", resp.Redirect)
	}
	return u.Query().Get("code")
}

func (e *signInEnv) exchange(code, verifier, redirect string) *httptest.ResponseRecorder {
	return e.do("/api/v1/devices/token", map[string]string{"code": code, "codeVerifier": verifier, "redirectUri": redirect}, "")
}

func wantGrantError(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"invalid_grant"`) {
		t.Errorf("%s: %d %s", what, rec.Code, rec.Body)
	}
}

func TestDeviceSignIn(t *testing.T) {
	e := newSignInEnv(t, nil)
	ctx := context.Background()
	verifier, challenge := pkce("abc")
	code := e.authorize(challenge)
	if len(code) != 43 {
		t.Fatalf("code %q", code)
	}
	// The code is kept hashed; no device exists until the exchange.
	var stored, devices int
	if err := e.h.Devices.db.QueryRow(ctx, `SELECT count(*) FROM device_codes WHERE code_hash = $1 AND expires_at BETWEEN now() + interval '119 seconds' AND now() + interval '121 seconds'`, hashToken(code)).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored code: %d, %v", stored, err)
	}
	if err := e.h.Devices.db.QueryRow(ctx, `SELECT count(*) FROM devices`).Scan(&devices); err != nil || devices != 0 {
		t.Fatalf("devices before exchange: %d, %v", devices, err)
	}

	rec := e.exchange(code, verifier, testRedirect)
	var resp struct {
		Token  string
		Device struct{ ID, Name, Platform string }
		User   struct{ ID, Username, DisplayName string }
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	if !strings.HasPrefix(resp.Token, DeviceTokenPrefix) || resp.Device.Name != "Ноутбук Василия" || resp.Device.Platform != "macos" || resp.User.ID != e.alice || resp.User.Username != "alice" {
		t.Fatalf("token response: %+v", resp)
	}

	// The token works as a device's.
	req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+resp.Token)
	me := httptest.NewRecorder()
	e.mux.ServeHTTP(me, req)
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), `"username":"alice"`) {
		t.Fatalf("me by new device: %d %s", me.Code, me.Body)
	}

	// Single use.
	wantGrantError(t, "replay", e.exchange(code, verifier, testRedirect))

	// Audited with what the device is, never the code or token.
	var action, target, details string
	if err := e.h.Devices.db.QueryRow(ctx, `SELECT action, target_name, details::text FROM audit_log WHERE action = 'device.authorize'`).Scan(&action, &target, &details); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if target != "Ноутбук Василия" || !strings.Contains(details, `"platform": "macos"`) || strings.Contains(details, resp.Token) || strings.Contains(details, code) {
		t.Fatalf("audit: %s %s %s", action, target, details)
	}
}

func TestDeviceAuthorizeValidates(t *testing.T) {
	e := newSignInEnv(t, nil)
	_, challenge := pkce("abc")
	for what, change := range map[string]func(map[string]any){
		"localhost":        func(b map[string]any) { b["redirectUri"] = "http://localhost:53682/cb" },
		"query":            func(b map[string]any) { b["redirectUri"] = testRedirect + "?a=b" },
		"low port":         func(b map[string]any) { b["redirectUri"] = "http://127.0.0.1:80/cb" },
		"remote":           func(b map[string]any) { b["redirectUri"] = "http://evil.example:53682/cb" },
		"plain method":     func(b map[string]any) { b["codeChallengeMethod"] = "plain" },
		"short challenge":  func(b map[string]any) { b["codeChallenge"] = challenge[:42] },
		"no state":         func(b map[string]any) { b["state"] = "" },
		"long state":       func(b map[string]any) { b["state"] = strings.Repeat("s", maxState+1) },
		"no name":          func(b map[string]any) { b["name"] = "  " },
		"long name":        func(b map[string]any) { b["name"] = strings.Repeat("я", maxAppName+1) },
		"platform":         func(b map[string]any) { b["platform"] = "android" },
		"long version":     func(b map[string]any) { b["clientVersion"] = strings.Repeat("1", maxClientVersion+1) },
		"deny bad uri":     func(b map[string]any) { b["deny"] = true; b["redirectUri"] = "http://localhost:53682/cb" },
		"deny empty state": func(b map[string]any) { b["deny"] = true; b["state"] = "" },
	} {
		body := grantBody(challenge)
		change(body)
		if rec := e.do("/api/v1/devices/authorize", body, e.session); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_request"`) {
			t.Errorf("%s: %d %s", what, rec.Code, rec.Body)
		}
	}
	var codes int
	if err := e.h.Devices.db.QueryRow(context.Background(), `SELECT count(*) FROM device_codes`).Scan(&codes); err != nil || codes != 0 {
		t.Fatalf("codes after refusals: %d, %v", codes, err)
	}

	// Refusing sends the client access_denied, and issues nothing.
	body := grantBody(challenge)
	body["deny"] = true
	rec := e.do("/api/v1/devices/authorize", body, e.session)
	var denied redirectResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &denied) != nil || denied.Redirect != testRedirect+"?error=access_denied&state=st%26ate%3D1" {
		t.Fatalf("deny: %d %s", rec.Code, rec.Body)
	}
	if err := e.h.Devices.db.QueryRow(context.Background(), `SELECT count(*) FROM device_codes`).Scan(&codes); err != nil || codes != 0 {
		t.Fatalf("codes after deny: %d, %v", codes, err)
	}

	// A session only: no one signed in, or a device, is turned away.
	if rec := e.do("/api/v1/devices/authorize", grantBody(challenge), ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	_, token, err := e.h.Devices.Create(context.Background(), e.alice, "PC", "linux", "1")
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.do("/api/v1/devices/authorize", grantBody(challenge), "Bearer "+token); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"device_forbidden"`) {
		t.Fatalf("by device: %d %s", rec.Code, rec.Body)
	}
}

func TestDeviceTokenRefuses(t *testing.T) {
	e := newSignInEnv(t, nil)
	ctx := context.Background()
	db := e.h.Devices.db
	verifier, challenge := pkce("abc")
	other, _ := pkce("xyz")

	wantGrantError(t, "unknown code", e.exchange(randomToken(), verifier, testRedirect))
	wantGrantError(t, "no code", e.exchange("", verifier, testRedirect))

	// A wrong verifier spends the code: the right one can't follow.
	code := e.authorize(challenge)
	wantGrantError(t, "wrong verifier", e.exchange(code, other, testRedirect))
	wantGrantError(t, "right verifier after a wrong one", e.exchange(code, verifier, testRedirect))

	code = e.authorize(challenge)
	wantGrantError(t, "malformed verifier", e.exchange(code, verifier+"/", testRedirect))

	code = e.authorize(challenge)
	wantGrantError(t, "other redirect", e.exchange(code, verifier, "http://127.0.0.1:53683/cb"))

	// Expired: refused, and swept by the next exchange.
	code = e.authorize(challenge)
	if _, err := db.Exec(ctx, `UPDATE device_codes SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	wantGrantError(t, "expired", e.exchange(code, verifier, testRedirect))
	var left int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM device_codes`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("codes left: %d, %v", left, err)
	}

	// Blocked between consent and exchange.
	code = e.authorize(challenge)
	if _, err := db.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, e.alice); err != nil {
		t.Fatal(err)
	}
	wantGrantError(t, "disabled", e.exchange(code, verifier, testRedirect))
	if _, err := db.Exec(ctx, `UPDATE users SET status = 'active' WHERE id = $1`, e.alice); err != nil {
		t.Fatal(err)
	}

	var devices int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM devices`).Scan(&devices); err != nil || devices != 0 {
		t.Fatalf("devices made by refused exchanges: %d, %v", devices, err)
	}
}

func TestDeviceSignInLimit(t *testing.T) {
	e := newSignInEnv(t, nil)
	ctx := context.Background()
	verifier, challenge := pkce("abc")
	for range MaxDevices - 1 {
		if _, _, err := e.h.Devices.Create(ctx, e.alice, "PC", "linux", "1"); err != nil {
			t.Fatal(err)
		}
	}
	// Room for one at consent, none left by the exchange.
	code := e.authorize(challenge)
	if _, _, err := e.h.Devices.Create(ctx, e.alice, "PC", "linux", "1"); err != nil {
		t.Fatal(err)
	}
	if rec := e.exchange(code, verifier, testRedirect); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"device_limit"`) {
		t.Fatalf("exchange over the limit: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("/api/v1/devices/authorize", grantBody(challenge), e.session); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"device_limit"`) {
		t.Fatalf("authorize over the limit: %d %s", rec.Code, rec.Body)
	}
}

func TestDeviceTokenRateLimit(t *testing.T) {
	e := newSignInEnv(t, ratelimit.New(time.Hour, 2))
	verifier, _ := pkce("abc")
	for i := range 2 {
		wantGrantError(t, "attempt "+string(rune('1'+i)), e.exchange(randomToken(), verifier, testRedirect))
	}
	rec := e.exchange(randomToken(), verifier, testRedirect)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("third attempt: %d %s", rec.Code, rec.Body)
	}
}
