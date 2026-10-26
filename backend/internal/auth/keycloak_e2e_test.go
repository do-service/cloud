package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/do-service/cloud/backend/internal/config"
	"github.com/do-service/cloud/backend/internal/db"
)

// The Keycloak end-to-end test runs only against a real Keycloak named by
// E2E_KEYCLOAK_URL (`make test-e2e`, the backend:e2e CI job), whose
// bootstrap admin is admin/admin as in docker-compose.yml. It sets up its
// own realm through the admin API, since a CI service can't mount a realm
// file from the repository.
const (
	e2eRealm        = "cloud-e2e"
	e2eClientSecret = "e2e-client-secret"
	e2eUser         = "ivan"
	e2ePassword     = "ivan-e2e-pass"
)

func TestKeycloakE2E(t *testing.T) {
	kc := strings.TrimRight(os.Getenv("E2E_KEYCLOAK_URL"), "/")
	if kc == "" {
		t.Skip("skipping: E2E_KEYCLOAK_URL not set")
	}
	// testPool skips without a database, which here would hide a broken job.
	if err := db.Migrate(testDatabaseURL()); err != nil {
		t.Fatalf("test database: %v", err)
	}
	pool := testPool(t)
	ctx := context.Background()

	var created []string
	h := &Handlers{
		Sessions: NewSessions(pool, 24*time.Hour),
		Users:    NewUsers(pool),
		OnUserCreated: func(_ context.Context, u User) error {
			created = append(created, u.Username)
			return nil
		},
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	admin := keycloakAdmin(t, kc)
	admin.createRealm(t, srv.URL+"/api/v1/auth/oidc/kc/callback")
	t.Cleanup(func() { keycloakAdmin(t, kc).deleteRealm(t) })

	providers := NewProviders(pool, testBox(t))
	spec := config.OIDCProvider{Slug: "kc", Name: "Keycloak", Issuer: kc + "/realms/" + e2eRealm, ClientID: "cloud",
		ClientSecret: e2eClientSecret, Scopes: []string{"openid", "profile", "email"}, AutoCreate: true}
	if err := providers.Sync(ctx, []config.OIDCProvider{spec}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	enabled, err := providers.Enabled(ctx)
	if err != nil {
		t.Fatalf("enabled: %v", err)
	}
	h.OIDC = NewOIDC(srv.URL+"/", enabled)
	h.Register(mux)

	first := browserSignIn(t, srv.URL, e2eUser, e2ePassword)
	if first.Username != e2eUser || first.DisplayName != "Ivan Petrov" || len(created) != 1 {
		t.Fatalf("first sign-in: %+v, created %v", first, created)
	}
	var identities int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identities WHERE user_id = $1 AND email = 'ivan@example.com'`, first.ID).Scan(&identities); err != nil || identities != 1 {
		t.Fatalf("identities: %d, %v", identities, err)
	}
	again := browserSignIn(t, srv.URL, e2eUser, e2ePassword)
	if again.ID != first.ID || len(created) != 1 {
		t.Fatalf("second sign-in: %+v, created %v", again, created)
	}
	if u := browserSignIn(t, srv.URL, e2eUser, "wrong"); u.ID != "" {
		t.Fatalf("wrong password signed in as %+v", u)
	}
}

var formAction = regexp.MustCompile(`<form[^>]*\saction="([^"]*)"`)

// browserSignIn walks the code flow as a browser does: /start, Keycloak's
// login form, back through /callback; it returns the signed-in user, or
// the zero User when Keycloak didn't let the user through.
func browserSignIn(t *testing.T, base, username, password string) User {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	app, _ := url.Parse(base)
	client := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		// Stop at the app's own page after /callback: there is no SPA here,
		// and where it sends the browser is what the test checks.
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Host == app.Host && !strings.HasPrefix(req.URL.Path, "/api/") {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	get := func(rawURL string) (*http.Response, string) {
		t.Helper()
		resp, err := client.Get(rawURL)
		if err != nil {
			t.Fatalf("GET %s: %v", rawURL, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	resp, page := get(base + "/api/v1/auth/oidc/kc/start?next=/files/docs")
	m := formAction.FindStringSubmatch(page)
	if resp.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("login page: %d %s, %.300s", resp.StatusCode, resp.Request.URL, page)
	}
	resp, err = client.PostForm(html.UnescapeString(m[1]), url.Values{"username": {username}, "password": {password}})
	if err != nil {
		t.Fatalf("submit login form: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK && !strings.HasPrefix(resp.Request.URL.String(), base) {
		// Keycloak shows its form again: wrong credentials.
		return User{}
	}
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || loc != "/files/docs" {
		t.Fatalf("after callback: %d → %q", resp.StatusCode, loc)
	}

	resp, body := get(base + "/api/v1/auth/me")
	var u User
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &u) != nil {
		t.Fatalf("me: %d %s", resp.StatusCode, body)
	}
	return u
}

type kcAdmin struct {
	base, token string
}

// keycloakAdmin logs in as the bootstrap admin, waiting for Keycloak to
// come up: in CI the test may start before the service is ready.
func keycloakAdmin(t *testing.T, base string) kcAdmin {
	t.Helper()
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {"admin"}}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		resp, err := http.PostForm(base+"/realms/master/protocol/openid-connect/token", form)
		if err == nil {
			var tok struct {
				AccessToken string `json:"access_token"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&tok)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil && tok.AccessToken != "" {
				return kcAdmin{base: base, token: tok.AccessToken}
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		if time.Now().After(deadline) {
			t.Fatalf("keycloak admin login at %s: %v", base, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func (a kcAdmin) do(t *testing.T, method, path string, body any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.base+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// createRealm makes the test realm afresh: one confidential client that
// requires PKCE S256, and one user with nothing left to set up at login.
func (a kcAdmin) createRealm(t *testing.T, redirectURI string) {
	t.Helper()
	a.deleteRealm(t) // left over from an interrupted run
	realm := map[string]any{
		"realm":   e2eRealm,
		"enabled": true,
		"clients": []map[string]any{{
			"clientId":                  "cloud",
			"enabled":                   true,
			"publicClient":              false,
			"secret":                    e2eClientSecret,
			"standardFlowEnabled":       true,
			"directAccessGrantsEnabled": false,
			"redirectUris":              []string{redirectURI},
			"attributes":                map[string]string{"pkce.code.challenge.method": "S256"},
		}},
		"users": []map[string]any{{
			"username":      e2eUser,
			"enabled":       true,
			"email":         "ivan@example.com",
			"emailVerified": true,
			"firstName":     "Ivan",
			"lastName":      "Petrov",
			"credentials":   []map[string]any{{"type": "password", "value": e2ePassword, "temporary": false}},
		}},
	}
	if code := a.do(t, http.MethodPost, "/admin/realms", realm); code != http.StatusCreated {
		t.Fatalf("create realm: %d", code)
	}
}

func (a kcAdmin) deleteRealm(t *testing.T) {
	t.Helper()
	if code := a.do(t, http.MethodDelete, "/admin/realms/"+e2eRealm, nil); code != http.StatusNoContent && code != http.StatusNotFound {
		t.Errorf("delete realm: %d", code)
	}
}
