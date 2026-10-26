package signin_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/testserver"
)

// The OS keyring is never touched: every test gets go-keyring's mock.
func TestMain(m *testing.M) {
	keyring.MockInit()
	testserver.Main(m)
}

// fakeCloud is the server's side of the sign-in: the consent page that
// redirects at once (the user said yes), and the code exchange checking
// PKCE and the redirect_uri as the real one does.
type fakeCloud struct {
	t   *testing.T
	srv *httptest.Server
	// answer builds the redirect's query from the request's; nil is a
	// code with the request's state.
	answer func(q url.Values) url.Values

	mu        sync.Mutex
	challenge string
	redirect  string
	query     url.Values
}

func newFakeCloud(t *testing.T) *fakeCloud {
	f := &fakeCloud{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCloud) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1/sync/info":
		_, _ = io.WriteString(w, `{"apiVersion":1,"serverVersion":"test","minClientVersion":"0.0.0","journalRetention":2592000,"maxWait":55}`)
	case "/devices/authorize":
		q := r.URL.Query()
		f.mu.Lock()
		f.query, f.challenge, f.redirect = q, q.Get("code_challenge"), q.Get("redirect_uri")
		f.mu.Unlock()
		answer := url.Values{"code": {"the-code"}, "state": {q.Get("state")}}
		if f.answer != nil {
			answer = f.answer(q)
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+answer.Encode(), http.StatusFound)
	case "/api/v1/devices/token":
		var req struct{ Code, CodeVerifier, RedirectURI string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		sum := sha256.Sum256([]byte(req.CodeVerifier))
		f.mu.Lock()
		ok := req.Code == "the-code" && base64.RawURLEncoding.EncodeToString(sum[:]) == f.challenge && req.RedirectURI == f.redirect
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":"invalid_grant","message":"no"}`)
			return
		}
		_, _ = io.WriteString(w, `{"token":"cld_secret","device":{"id":"d1","name":"Мой Mac","platform":"macos"},"user":{"id":"u1","username":"ivan","displayName":"Иван"}}`)
	default:
		http.NotFound(w, r)
	}
}

// browser follows the consent page's redirect back to the client, as the
// system browser would, and hands over the page it ends on.
type browser struct {
	pages chan string
}

func newBrowser() *browser { return &browser{pages: make(chan string, 1)} }

func (b *browser) open(u string) error {
	go func() {
		res, err := http.Get(u)
		if err != nil {
			b.pages <- "error: " + err.Error()
			return
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		b.pages <- string(body)
	}()
	return nil
}

func (b *browser) page(t *testing.T) string {
	t.Helper()
	select {
	case p := <-b.pages:
		return p
	case <-time.After(10 * time.Second):
		t.Fatal("the browser got no page")
		return ""
	}
}

func TestLogin(t *testing.T) {
	f := newFakeCloud(t)
	b := newBrowser()
	var shown string
	res, err := signin.Login(context.Background(), signin.Options{
		Server: f.srv.URL + "/", DeviceName: "Мой Mac", ClientVersion: "1.2.3",
		OpenBrowser: b.open, OnURL: func(u string) { shown = u },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Server != f.srv.URL || res.Token != "cld_secret" || res.User.ID != "u1" || res.Device.ID != "d1" {
		t.Errorf("result = %+v", res)
	}
	if page := b.page(t); !strings.Contains(page, "Готово, можно вернуться в приложение") {
		t.Errorf("page = %s", page)
	}
	q := f.query
	if !strings.HasPrefix(shown, f.srv.URL+"/devices/authorize?") || q.Get("code_challenge_method") != "S256" ||
		len(q.Get("code_challenge")) != 43 || q.Get("state") == "" || q.Get("name") != "Мой Mac" ||
		q.Get("platform") != signin.Platform() || q.Get("client_version") != "1.2.3" {
		t.Errorf("authorize URL %s", shown)
	}
	if ru, err := url.Parse(q.Get("redirect_uri")); err != nil || ru.Hostname() != "127.0.0.1" || ru.Path != "/cb" || ru.Port() == "" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
}

func TestLoginFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		answer func(q url.Values) url.Values
		want   error
		page   string
	}{
		"state mismatch": {
			answer: func(q url.Values) url.Values { return url.Values{"code": {"the-code"}, "state": {"other"}} },
			want:   signin.ErrStateMismatch, page: "не от текущего входа",
		},
		"access denied": {
			answer: func(q url.Values) url.Values {
				return url.Values{"error": {"access_denied"}, "state": {q.Get("state")}}
			},
			want: signin.ErrAccessDenied, page: "доступ не разрешён",
		},
		"code refused": {
			answer: func(q url.Values) url.Values { return url.Values{"code": {"wrong"}, "state": {q.Get("state")}} },
			want:   api.ErrInvalidGrant, page: "устарел или уже использован",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeCloud(t)
			f.answer = tc.answer
			b := newBrowser()
			_, err := signin.Login(context.Background(), signin.Options{Server: f.srv.URL, DeviceName: "x", OpenBrowser: b.open})
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if page := b.page(t); !strings.Contains(page, tc.page) {
				t.Errorf("page = %s", page)
			}
		})
	}
}

func TestLoginWaits(t *testing.T) {
	f := newFakeCloud(t)
	var redirect string
	idle := func(u string) error {
		pu, _ := url.Parse(u)
		redirect = pu.Query().Get("redirect_uri")
		return nil
	}

	_, err := signin.Login(context.Background(), signin.Options{Server: f.srv.URL, DeviceName: "x", OpenBrowser: idle, Timeout: 100 * time.Millisecond})
	if !errors.Is(err, signin.ErrTimeout) {
		t.Errorf("timeout: %v", err)
	}
	// The loopback listener is gone with the sign-in.
	if _, err := http.Get(redirect + "?code=late"); err == nil {
		t.Errorf("the redirect_uri still answers after the timeout")
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := signin.Login(ctx, signin.Options{Server: f.srv.URL, DeviceName: "x", OpenBrowser: idle}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: %v", err)
	}

	failing := func(string) error { return errors.New("no browser") }
	if _, err := signin.Login(context.Background(), signin.Options{Server: f.srv.URL, DeviceName: "x", OpenBrowser: failing}); err == nil {
		t.Error("no browser and no OnURL: want an error")
	}
}

func TestLoginChecksServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"apiVersion":2}`)
	}))
	defer srv.Close()
	opened := false
	_, err := signin.Login(context.Background(), signin.Options{Server: srv.URL, OpenBrowser: func(string) error { opened = true; return nil }})
	if !errors.Is(err, api.ErrIncompatible) || opened {
		t.Errorf("err = %v, browser opened: %v", err, opened)
	}
	if _, err := signin.Login(context.Background(), signin.Options{Server: "http://cloud.example.com"}); err == nil {
		t.Error("plain http to another host: want an error")
	}
}

func result(server, userID string) signin.Result {
	return signin.Result{Server: server, DeviceToken: api.DeviceToken{
		Token: "cld_" + userID, Device: api.Device{ID: "d-" + userID, Name: "Mac"}, User: api.User{ID: userID, Username: "user-" + userID},
	}}
}

func TestStore(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	s := signin.NewStore(dir)
	// Two accounts on one server and one on another: each its own token.
	for _, r := range []signin.Result{result("https://a.example", "u1"), result("https://a.example", "u2"), result("https://b.example", "u1")} {
		if _, err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	if tok, err := keyring.Get(signin.KeyringService, "https://a.example|u2"); err != nil || tok != "cld_u2" {
		t.Errorf("keyring entry: %q, %v", tok, err)
	}
	ts := s.TokenSource("https://b.example", "u1")
	if tok, err := ts.Token(context.Background()); err != nil || tok != "cld_u1" {
		t.Errorf("token source: %q, %v", tok, err)
	}
	accounts, err := s.Accounts()
	if err != nil || len(accounts) != 3 || accounts[0].Username != "user-u1" || accounts[0].TokenInFile {
		t.Fatalf("accounts = %+v, %v", accounts, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, signin.AccountsFile))
	if strings.Contains(string(raw), "cld_") {
		t.Errorf("a token in %s", signin.AccountsFile)
	}
	if _, err := os.Stat(filepath.Join(dir, signin.TokensFile)); !os.IsNotExist(err) {
		t.Errorf("%s with a working keyring: %v", signin.TokensFile, err)
	}

	// Signing in again replaces the account.
	again := result("https://a.example", "u1")
	again.Token = "cld_new"
	if _, err := s.Save(again); err != nil {
		t.Fatal(err)
	}
	if tok, _ := s.Token("https://a.example", "u1"); tok != "cld_new" {
		t.Errorf("token after a second sign-in = %q", tok)
	}
	if accounts, _ := s.Accounts(); len(accounts) != 3 {
		t.Errorf("accounts after a second sign-in = %+v", accounts)
	}

	if err := s.Delete("https://a.example", "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token("https://a.example", "u1"); !errors.Is(err, signin.ErrNoToken) {
		t.Errorf("token after delete: %v", err)
	}
	if accounts, _ := s.Accounts(); len(accounts) != 2 {
		t.Errorf("accounts after delete = %+v", accounts)
	}
}

func TestStoreWithoutKeyring(t *testing.T) {
	keyring.MockInitWithError(errors.New("org.freedesktop.secrets was not provided by any .service files"))
	t.Cleanup(keyring.MockInit)
	dir := t.TempDir()
	s := signin.NewStore(dir)
	a, err := s.Save(result("https://a.example", "u1"))
	if err != nil {
		t.Fatal(err)
	}
	if !a.TokenInFile {
		t.Error("account doesn't say its token is in a file")
	}
	// Windows has no mode bits: the file is private by the ACL of the
	// user's own profile directory.
	fi, err := os.Stat(filepath.Join(dir, signin.TokensFile))
	if err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v, %v", signin.TokensFile, fi, err)
	}
	if tok, err := s.Token("https://a.example", "u1"); err != nil || tok != "cld_u1" {
		t.Errorf("token = %q, %v", tok, err)
	}
	if err := s.Delete("https://a.example", "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, signin.TokensFile)); !os.IsNotExist(err) {
		t.Errorf("%s left after the last token went: %v", signin.TokensFile, err)
	}
}

// TestLoginRealServer runs the whole sign-in against the real server:
// the "browser" does what the consent page does (POST
// /api/v1/devices/authorize with its session) and follows the redirect.
func TestLoginRealServer(t *testing.T) {
	keyring.MockInit()
	srv := testserver.Start(t)
	u := srv.CreateUser(t, "signin", 0)
	cookie := srv.Login(t, u.Username, u.Password)
	b := newBrowser()
	consent := func(page string) error {
		pu, err := url.Parse(page)
		if err != nil {
			return err
		}
		q := pu.Query()
		redirect, err := srv.Authorize(cookie, q.Get("redirect_uri"), q.Get("code_challenge"), q.Get("state"), q.Get("name"))
		if errors.Is(err, testserver.ErrNoDeviceSignIn) {
			b.pages <- "skip"
			return err
		}
		if err != nil {
			return err
		}
		return b.open(redirect)
	}
	res, err := signin.Login(context.Background(), signin.Options{Server: srv.URL, DeviceName: "Тестовый Mac", OpenBrowser: consent})
	if errors.Is(err, testserver.ErrNoDeviceSignIn) {
		t.Skip("E12-2 isn't on the server yet")
	}
	if err != nil {
		t.Fatal(err)
	}
	if res.User.ID != u.ID || res.User.Username != u.Username || res.Device.Name != "Тестовый Mac" || !strings.HasPrefix(res.Token, "cld_") {
		t.Errorf("result: user %+v, device %+v", res.User, res.Device)
	}
	if page := b.page(t); !strings.Contains(page, "Готово") {
		t.Errorf("page = %s", page)
	}

	s := signin.NewStore(t.TempDir())
	if _, err := s.Save(res); err != nil {
		t.Fatal(err)
	}
	c, err := api.New(srv.URL, api.Options{Token: s.TokenSource(res.Server, res.User.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Roots(context.Background()); err != nil {
		t.Fatalf("roots with the new token: %v", err)
	}
	if err := s.Logout(context.Background(), srv.URL, res.User.ID, signin.LogoutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Roots(context.Background()); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("roots after logout: %v, want ErrUnauthorized", err)
	}
	if accounts, _ := s.Accounts(); len(accounts) != 0 {
		t.Errorf("accounts after logout = %+v", accounts)
	}
}
