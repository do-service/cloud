package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/config"
)

// fakeIdP is just enough of an OpenID provider for the code flow:
// discovery, keys, and a token endpoint that checks PKCE and hands out
// whatever ID token claims the test granted for a code.
type fakeIdP struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu     sync.Mutex
	grants map[string]grant
}

type grant struct {
	challenge string
	claims    map[string]any
	signer    *rsa.PrivateKey
}

const testClientID = "cloud"

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key, grants: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/auth",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		g, ok := f.grants[r.PostForm.Get("code")]
		delete(f.grants, r.PostForm.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || b64(sum[:]) != g.challenge {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 300,
			"id_token": signJWT(t, g.signer, g.claims),
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func signJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signing := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + b64(sig)
}

// grant registers the claims the next token request with code will get;
// the defaults make a valid ID token for nonce and sub.
func (f *fakeIdP) grant(challenge, nonce, sub string, override map[string]any) string {
	claims := map[string]any{
		"iss": f.srv.URL, "aud": testClientID, "sub": sub, "nonce": nonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
	}
	signer := f.key
	for k, v := range override {
		if k == "signer" {
			signer = v.(*rsa.PrivateKey)
			continue
		}
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	code := randomToken()
	f.mu.Lock()
	f.grants[code] = grant{challenge: challenge, claims: claims, signer: signer}
	f.mu.Unlock()
	return code
}

type oidcEnv struct {
	pool    *pgxpool.Pool
	idp     *fakeIdP
	h       *Handlers
	mux     *http.ServeMux
	created []string
}

const testBaseURL = "https://cloud.example.com"

// newOIDCEnv serves one provider "kc" backed by a fake IdP; adjust tweaks
// its configuration.
func newOIDCEnv(t *testing.T, adjust func(*config.OIDCProvider)) *oidcEnv {
	t.Helper()
	pool := testPool(t)
	idp := newFakeIdP(t)
	spec := config.OIDCProvider{Slug: "kc", Name: "Keycloak", Issuer: idp.srv.URL, ClientID: testClientID,
		ClientSecret: "s3cret", Scopes: []string{"openid", "profile", "email"}, AutoCreate: true}
	if adjust != nil {
		adjust(&spec)
	}
	providers := NewProviders(pool, testBox(t))
	ctx := context.Background()
	if err := providers.Sync(ctx, []config.OIDCProvider{spec}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	enabled, err := providers.Enabled(ctx)
	if err != nil {
		t.Fatalf("enabled: %v", err)
	}
	e := &oidcEnv{pool: pool, idp: idp, mux: http.NewServeMux()}
	e.h = &Handlers{
		Sessions: NewSessions(pool, 24*time.Hour),
		Cookies:  Cookies{Secure: true},
		Users:    NewUsers(pool),
		OIDC:     NewOIDC(testBaseURL+"/", enabled),
		OnUserCreated: func(_ context.Context, u User) error {
			e.created = append(e.created, u.Username)
			return nil
		},
	}
	e.h.Register(e.mux)
	return e
}

func testBox(t *testing.T) *SecretBox {
	t.Helper()
	box, err := NewSecretBox([]byte(strings.Repeat("k", config.SecretKeyLen)))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func (e *oidcEnv) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// started is a flow begun at /start, as the browser holds it.
type started struct {
	cookie                    *http.Cookie
	state, nonce, challenge   string
	redirectURI, authLocation string
}

func (e *oidcEnv) start(t *testing.T, next string) started {
	t.Helper()
	rec := e.do(httptest.NewRequest("GET", testBaseURL+"/api/v1/auth/oidc/kc/start?next="+url.QueryEscape(next), nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != hostFlowCookieName || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatalf("flow cookie: %+v", cookies)
	}
	return started{cookie: cookies[0], state: q.Get("state"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
		redirectURI: q.Get("redirect_uri"), authLocation: rec.Header().Get("Location")}
}

func (e *oidcEnv) callback(cookie *http.Cookie, query url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/v1/auth/oidc/kc/callback?"+query.Encode(), nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return e.do(req)
}

// signIn runs the whole flow for sub with extra ID token claims.
func (e *oidcEnv) signIn(t *testing.T, sub string, claims map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	s := e.start(t, "/files")
	code := e.idp.grant(s.challenge, s.nonce, sub, claims)
	return e.callback(s.cookie, url.Values{"code": {code}, "state": {s.state}})
}

func sessionCookie(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == hostCookieName && c.MaxAge >= 0 {
			return c.Value
		}
	}
	return ""
}

func (e *oidcEnv) me(t *testing.T, rec *httptest.ResponseRecorder) User {
	t.Helper()
	raw := sessionCookie(rec)
	if raw == "" {
		t.Fatalf("no session cookie; %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
	u, _, err := e.h.Sessions.Lookup(context.Background(), raw)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	return u
}

func TestOIDCSignIn(t *testing.T) {
	e := newOIDCEnv(t, nil)
	rec := e.do(httptest.NewRequest("GET", "/api/v1/auth/providers", nil))
	if body := rec.Body.String(); !strings.Contains(body, `"password":true`) || !strings.Contains(body, `{"slug":"kc","name":"Keycloak"}`) {
		t.Fatalf("providers: %s", body)
	}

	s := e.start(t, "/files/docs?sort=name")
	if !strings.HasPrefix(s.authLocation, e.idp.srv.URL+"/auth?") || s.state == "" || s.nonce == "" || s.challenge == "" {
		t.Fatalf("auth redirect: %s", s.authLocation)
	}
	if loc, _ := url.Parse(s.authLocation); loc.Query().Get("code_challenge_method") != "S256" || loc.Query().Get("client_id") != testClientID {
		t.Fatalf("auth redirect: %s", s.authLocation)
	}
	if s.redirectURI != testBaseURL+"/api/v1/auth/oidc/kc/callback" {
		t.Fatalf("redirect_uri %q", s.redirectURI)
	}

	code := e.idp.grant(s.challenge, s.nonce, "sub-1", map[string]any{
		"preferred_username": "vasily", "name": "Василий", "email": "v@example.com", "email_verified": "true",
	})
	rec = e.callback(s.cookie, url.Values{"code": {code}, "state": {s.state}})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/files/docs?sort=name" {
		t.Fatalf("callback: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	first := e.me(t, rec)
	if first.Username != "vasily" || first.DisplayName != "Василий" || first.IsAdmin {
		t.Fatalf("user: %+v", first)
	}
	var email *string
	if err := e.pool.QueryRow(context.Background(), `SELECT email::text FROM users WHERE id = $1`, first.ID).Scan(&email); err != nil || email == nil || *email != "v@example.com" {
		t.Fatalf("verified email kept: %v, %v", email, err)
	}

	// The same subject comes back to the same account, set up only once,
	// whatever its username claim says now.
	again := e.me(t, e.signIn(t, "sub-1", map[string]any{"preferred_username": "renamed"}))
	if again.ID != first.ID || len(e.created) != 1 {
		t.Fatalf("second sign-in: %+v, created %v", again, e.created)
	}
	// The desktop sign-in's consent page comes back whole, its query and
	// all: the client's redirect_uri, challenge and state ride along.
	authorize := "/devices/authorize?" + url.Values{
		"redirect_uri": {"http://127.0.0.1:53682/cb"}, "code_challenge": {strings.Repeat("A", 43)},
		"code_challenge_method": {"S256"}, "state": {"a&b=c"}, "name": {"Ноутбук"}, "platform": {"macos"}, "client_version": {"0.1.0"},
	}.Encode()
	s2 := e.start(t, authorize)
	code2 := e.idp.grant(s2.challenge, s2.nonce, "sub-1", nil)
	if rec := e.callback(s2.cookie, url.Values{"code": {code2}, "state": {s2.state}}); rec.Code != http.StatusFound || rec.Header().Get("Location") != authorize {
		t.Fatalf("authorize round trip: %d → %q, want %q", rec.Code, rec.Header().Get("Location"), authorize)
	}
	// A flow cookie works once: replaying the callback fails.
	if rec := e.callback(nil, url.Values{"code": {code}, "state": {s.state}}); rec.Header().Get("Location") != "/login?error=oidc_failed" {
		t.Fatalf("replay: %q", rec.Header().Get("Location"))
	}
}

func TestOIDCRejects(t *testing.T) {
	e := newOIDCEnv(t, nil)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		run  func(s started) *httptest.ResponseRecorder
	}{
		{"state mismatch", func(s started) *httptest.ResponseRecorder {
			return e.callback(s.cookie, url.Values{"code": {e.idp.grant(s.challenge, s.nonce, "x", nil)}, "state": {"forged"}})
		}},
		{"no flow cookie", func(s started) *httptest.ResponseRecorder {
			return e.callback(nil, url.Values{"code": {e.idp.grant(s.challenge, s.nonce, "x", nil)}, "state": {s.state}})
		}},
		{"provider error", func(s started) *httptest.ResponseRecorder {
			return e.callback(s.cookie, url.Values{"error": {"access_denied"}, "state": {s.state}})
		}},
		{"pkce mismatch", func(s started) *httptest.ResponseRecorder {
			return e.callback(s.cookie, url.Values{"code": {e.idp.grant("other-challenge", s.nonce, "x", nil)}, "state": {s.state}})
		}},
		{"nonce mismatch", func(s started) *httptest.ResponseRecorder {
			return e.callback(s.cookie, url.Values{"code": {e.idp.grant(s.challenge, "other-nonce", "x", nil)}, "state": {s.state}})
		}},
		{"wrong audience", func(s started) *httptest.ResponseRecorder {
			return e.callback(s.cookie, url.Values{"code": {e.idp.grant(s.challenge, s.nonce, "x", map[string]any{"aud": "someone-else"})}, "state": {s.state}})
		}},
		{"wrong issuer", func(s started) *httptest.ResponseRecorder {
			return e.callback(s.cookie, url.Values{"code": {e.idp.grant(s.challenge, s.nonce, "x", map[string]any{"iss": "https://evil.example.com"})}, "state": {s.state}})
		}},
		{"expired", func(s started) *httptest.ResponseRecorder {
			return e.callback(s.cookie, url.Values{"code": {e.idp.grant(s.challenge, s.nonce, "x", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})}, "state": {s.state}})
		}},
		{"bad signature", func(s started) *httptest.ResponseRecorder {
			return e.callback(s.cookie, url.Values{"code": {e.idp.grant(s.challenge, s.nonce, "x", map[string]any{"signer": otherKey})}, "state": {s.state}})
		}},
		{"tampered cookie", func(s started) *httptest.ResponseRecorder {
			c := *s.cookie
			c.Value = "!!" + c.Value
			return e.callback(&c, url.Values{"code": {e.idp.grant(s.challenge, s.nonce, "x", nil)}, "state": {s.state}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.run(e.start(t, "/"))
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?error=oidc_failed" || sessionCookie(rec) != "" {
				t.Fatalf("%d → %q, session %q", rec.Code, rec.Header().Get("Location"), sessionCookie(rec))
			}
		})
	}
	var users int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&users); err != nil || users != 0 {
		t.Fatalf("users created by failed flows: %d, %v", users, err)
	}
	if rec := e.do(httptest.NewRequest("GET", "/api/v1/auth/oidc/nope/start", nil)); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown provider: %d", rec.Code)
	}
}

func TestOIDCLinkByEmail(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		link     bool
		verified any
		linked   bool
	}{
		{"linking off", false, true, false},
		{"unverified email", true, false, false},
		{"verified email", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newOIDCEnv(t, func(p *config.OIDCProvider) { p.LinkByEmail = tc.link })
			alice, err := e.h.Users.CreateLocal(ctx, "alice", "", "password1", false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.pool.Exec(ctx, `UPDATE users SET email = 'Alice@Example.com' WHERE id = $1`, alice.ID); err != nil {
				t.Fatal(err)
			}
			u := e.me(t, e.signIn(t, "sub-a", map[string]any{"email": "alice@example.com", "email_verified": tc.verified}))
			if (u.ID == alice.ID) != tc.linked {
				t.Fatalf("signed in as %+v, alice is %s", u, alice.ID)
			}
			if !tc.linked {
				// The new account must not carry an unverified address, or
				// it would be the link target for the real owner later.
				var email *string
				if err := e.pool.QueryRow(ctx, `SELECT email::text FROM users WHERE id = $1`, u.ID).Scan(&email); err != nil {
					t.Fatal(err)
				}
				if (email != nil) != (tc.verified == true) {
					t.Fatalf("email of new account: %v", email)
				}
			}
		})
	}

	t.Run("ambiguous", func(t *testing.T) {
		e := newOIDCEnv(t, func(p *config.OIDCProvider) { p.LinkByEmail = true })
		if _, err := e.pool.Exec(ctx, `INSERT INTO users (username, email) VALUES ('a1', 'same@example.com'), ('a2', 'same@example.com')`); err != nil {
			t.Fatal(err)
		}
		rec := e.signIn(t, "sub-s", map[string]any{"email": "same@example.com", "email_verified": true})
		if rec.Header().Get("Location") != "/login?error=oidc_failed" {
			t.Fatalf("ambiguous email: %q", rec.Header().Get("Location"))
		}
	})
}

func TestOIDCAccountRules(t *testing.T) {
	ctx := context.Background()
	t.Run("signup disabled", func(t *testing.T) {
		e := newOIDCEnv(t, func(p *config.OIDCProvider) { p.AutoCreate = false })
		if rec := e.signIn(t, "sub-n", nil); rec.Header().Get("Location") != "/login?error=signup_disabled" {
			t.Fatalf("got %q", rec.Header().Get("Location"))
		}
	})
	t.Run("account disabled", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		u := e.me(t, e.signIn(t, "sub-d", nil))
		if _, err := e.pool.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, u.ID); err != nil {
			t.Fatal(err)
		}
		if rec := e.signIn(t, "sub-d", nil); rec.Header().Get("Location") != "/login?error=account_disabled" || sessionCookie(rec) != "" {
			t.Fatalf("got %q", rec.Header().Get("Location"))
		}
	})
	t.Run("username taken", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		if _, err := e.h.Users.CreateLocal(ctx, "Alice", "", "password1", false); err != nil {
			t.Fatal(err)
		}
		u := e.me(t, e.signIn(t, "sub-u", map[string]any{"preferred_username": "alice"}))
		if u.Username != "alice-2" {
			t.Fatalf("username %q", u.Username)
		}
	})
	t.Run("setup fails", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		e.h.OnUserCreated = func(context.Context, User) error { return errors.New("disk full") }
		if rec := e.signIn(t, "sub-f", nil); rec.Header().Get("Location") != "/login?error=internal_error" {
			t.Fatalf("got %q", rec.Header().Get("Location"))
		}
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("half-set-up account left behind: %d, %v", n, err)
		}
	})
	t.Run("concurrent first sign-in", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		p := e.h.OIDC.bySlug["kc"].Provider
		var wg sync.WaitGroup
		ids := make([]string, 8)
		errs := make([]error, 8)
		for i := range ids {
			wg.Go(func() {
				u, _, err := e.h.Users.SignInOIDC(ctx, p, Claims{Subject: "sub-c", PreferredUsername: "carol"})
				ids[i], errs[i] = u.ID, err
			})
		}
		wg.Wait()
		for i := range ids {
			if errs[i] != nil || ids[i] != ids[0] {
				t.Fatalf("sign-in %d: %s, %v (first %s)", i, ids[i], errs[i], ids[0])
			}
		}
	})
}

func TestOIDCProviderUnavailable(t *testing.T) {
	e := newOIDCEnv(t, nil)
	e.idp.srv.Close()
	for range 2 {
		rec := e.do(httptest.NewRequest("GET", testBaseURL+"/api/v1/auth/oidc/kc/start", nil))
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?error=provider_unavailable" || len(rec.Result().Cookies()) != 0 {
			t.Fatalf("%d → %q", rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/files/a?b=c":          "/files/a?b=c",
		"":                      "/",
		"files":                 "/",
		"//evil.example.com":    "/",
		"/\\evil.example.com":   "/",
		"https://evil.example":  "/",
		"/x\r\nSet-Cookie: a=b": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUsernameBase(t *testing.T) {
	for _, tc := range []struct {
		c    Claims
		want string
	}{
		{Claims{PreferredUsername: "vasily"}, "vasily"},
		{Claims{PreferredUsername: "Vasily  Ivanov"}, "Vasily-Ivanov"},
		{Claims{PreferredUsername: "Василий", Email: "v.ivanov@example.com"}, "v.ivanov"},
		{Claims{PreferredUsername: "_.bob"}, "bob"},
		{Claims{PreferredUsername: "--x--"}, "x"},
		{Claims{Name: "Только имя"}, "user"},
		{Claims{PreferredUsername: strings.Repeat("a", 100)}, strings.Repeat("a", maxUsernameBase)},
	} {
		if got := usernameBase(tc.c); got != tc.want {
			t.Errorf("usernameBase(%+v) = %q, want %q", tc.c, got, tc.want)
		}
	}
}

func TestProvidersSync(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	providers := NewProviders(pool, testBox(t))
	specs := []config.OIDCProvider{
		{Slug: "kc", Name: "Keycloak", Issuer: "https://id.example.com", ClientID: "cloud", ClientSecret: "s3cret", Scopes: []string{"openid"}, AutoCreate: true},
		{Slug: "gl", Name: "GitLab", Issuer: "https://gitlab.example.com", ClientID: "pub", Scopes: []string{"openid", "email"}},
	}
	if err := providers.Sync(ctx, specs); err != nil {
		t.Fatal(err)
	}
	got, err := providers.Enabled(ctx)
	if err != nil || len(got) != 2 || got[0].Slug != "gl" || got[1].ClientSecret != "s3cret" || got[0].ClientSecret != "" {
		t.Fatalf("enabled: %+v, %v", got, err)
	}
	var leaked bool
	if err := pool.QueryRow(ctx, `SELECT position('s3cret'::bytea IN client_secret_enc) > 0 FROM auth_providers WHERE slug = 'kc'`).Scan(&leaked); err != nil || leaked {
		t.Fatalf("secret stored in the clear: %v, %v", leaked, err)
	}

	// Dropped from the env: disabled, not deleted; listed again: back.
	if err := providers.Sync(ctx, specs[:1]); err != nil {
		t.Fatal(err)
	}
	if got, _ := providers.Enabled(ctx); len(got) != 1 || got[0].Slug != "kc" {
		t.Fatalf("after drop: %+v", got)
	}
	if err := providers.Sync(ctx, specs); err != nil {
		t.Fatal(err)
	}
	if got, _ := providers.Enabled(ctx); len(got) != 2 {
		t.Fatalf("after re-adding: %+v", got)
	}

	// Another key can't open the stored secret, and no key can't either.
	other, err := NewSecretBox([]byte(strings.Repeat("o", config.SecretKeyLen)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewProviders(pool, other).Enabled(ctx); err == nil {
		t.Fatal("secret opened with the wrong key")
	}
	none, _ := NewSecretBox(nil)
	if _, err := NewProviders(pool, none).Enabled(ctx); !errors.Is(err, ErrNoSecretKey) {
		t.Fatalf("without key: %v", err)
	}
}

func TestSecretBox(t *testing.T) {
	box := testBox(t)
	sealed, err := box.Seal("s3cret", "kc")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := box.Open(sealed, "kc"); err != nil || got != "s3cret" {
		t.Fatalf("open: %q, %v", got, err)
	}
	// Bound to its row: the same value under another slug won't open.
	if _, err := box.Open(sealed, "gl"); err == nil {
		t.Fatal("opened under another context")
	}
	again, _ := box.Seal("s3cret", "kc")
	if string(again) == string(sealed) {
		t.Fatal("sealing is deterministic")
	}
	if _, err := box.Open(sealed[:5], "kc"); err == nil {
		t.Fatal("opened a truncated value")
	}
}

// A sign-in begun on another name for the server goes to PUBLIC_BASE_URL's
// first: the provider comes back there, and the flow cookie must be there.
func TestOIDCStartsOnPublicHost(t *testing.T) {
	e := newOIDCEnv(t, nil)
	rec := e.do(httptest.NewRequest("GET", "http://127.0.0.1:8090/api/v1/auth/oidc/kc/start?next=%2Ffiles", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != testBaseURL+"/api/v1/auth/oidc/kc/start?canonical=1&next=%2Ffiles" {
		t.Fatalf("start elsewhere: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("flow cookie set on the wrong host: %v", rec.Result().Cookies())
	}
	// Once only: behind a proxy that passes another Host, it carries on.
	rec = e.do(httptest.NewRequest("GET", "http://cloud:8080/api/v1/auth/oidc/kc/start?canonical=1&next=%2Ffiles", nil))
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), e.idp.srv.URL) || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("second start: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}
