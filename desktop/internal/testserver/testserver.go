// Package testserver runs the real cloud server (../backend/cmd/server)
// for the client's integration tests: the Go code isn't shared with the
// backend, so the contract is held by tests against the server itself
// (docs/desktop-client.md).
//
// The server gets a throwaway database on the compose postgres
// (127.0.0.1:55433, or TEST_POSTGRES_URL: any database there, used to
// create and drop ours) and a temporary data dir, like the backend's
// make test-webdav. Without a reachable postgres the tests skip. CI's
// desktop:test has a postgres service for them (TEST_POSTGRES_URL).
//
// One server per test binary, started by the first test that asks and
// stopped by Main: call it from TestMain.
package testserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	defaultPostgres = "postgres://cloud:cloud@127.0.0.1:55433/postgres?sslmode=disable"
	AdminUsername   = "admin"
	// AdminPassword is only ever this throwaway server's.
	AdminPassword = "desktop-test-admin"
)

type Server struct {
	// URL is http://127.0.0.1:<port>.
	URL string

	dir    string
	dbName string
	dbURL  string
	pgURL  string
	cmd    *exec.Cmd
	exited chan struct{}
	hc     *http.Client

	mu          sync.Mutex
	adminCookie string
	sessions    map[string]string
}

var (
	once    sync.Once
	shared  *Server
	skipMsg string
	failErr error
)

// Start returns the shared server, starting it on first use. It skips t
// when postgres isn't reachable.
func Start(t testing.TB) *Server {
	t.Helper()
	once.Do(func() { shared, skipMsg, failErr = start() })
	if skipMsg != "" {
		t.Skip(skipMsg)
	}
	if failErr != nil {
		t.Fatal(failErr)
	}
	return shared
}

// Main runs the tests and then stops the server and drops its database.
func Main(m *testing.M) {
	code := m.Run()
	if shared != nil {
		shared.stop()
	}
	os.Exit(code)
}

func postgresURL() string {
	if u := os.Getenv("TEST_POSTGRES_URL"); u != "" {
		return u
	}
	return defaultPostgres
}

func start() (*Server, string, error) {
	pgURL := postgresURL()
	cfg, err := pgconn.ParseConfig(pgURL)
	if err != nil {
		return nil, "", fmt.Errorf("TEST_POSTGRES_URL: %w", err)
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port)), 2*time.Second)
	if err != nil {
		return nil, fmt.Sprintf("postgres not reachable at %s:%d (docker compose up -d postgres): %v", cfg.Host, cfg.Port, err), nil
	}
	conn.Close()

	s := &Server{pgURL: pgURL, exited: make(chan struct{}), hc: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}}
	if s.dir, err = os.MkdirTemp("", "cloud-desktop-testserver-"); err != nil {
		return nil, "", err
	}
	fail := func(err error) (*Server, string, error) {
		s.stop()
		return nil, "", err
	}
	bin := filepath.Join(s.dir, "server")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "./cmd/server")
	build.Dir = backendDir()
	if out, err := build.CombinedOutput(); err != nil {
		return fail(fmt.Errorf("build the server: %v\n%s", err, out))
	}

	s.dbName = "cloud_desktop_" + randomHex(6)
	if err := s.exec(context.Background(), "CREATE DATABASE "+s.dbName); err != nil {
		s.dbName = ""
		return fail(fmt.Errorf("create the test database: %w", err))
	}
	dbURL, err := withDatabase(pgURL, s.dbName)
	if err != nil {
		return fail(err)
	}
	s.dbURL = dbURL
	port, err := freePort()
	if err != nil {
		return fail(err)
	}
	s.URL = "http://127.0.0.1:" + port

	logs, err := os.Create(filepath.Join(s.dir, "server.log"))
	if err != nil {
		return fail(err)
	}
	s.cmd = exec.Command(bin)
	s.cmd.Dir = s.dir
	// A clean environment: a developer's OIDC_PROVIDERS or DATABASE_URL
	// mustn't reach the throwaway server.
	s.cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + s.dir, "TMPDIR=" + os.TempDir(),
		"HTTP_ADDR=127.0.0.1:" + port, "METRICS_ADDR=", "PUBLIC_BASE_URL=" + s.URL,
		"DATABASE_URL=" + dbURL, "DATA_DIR=" + filepath.Join(s.dir, "data"),
		"ADMIN_USERNAME=" + AdminUsername, "ADMIN_PASSWORD=" + AdminPassword,
		"WOPI_DISCOVERY_URL=", "LOG_LEVEL=warn",
		// So DeviceToken can name a client address of its own per code
		// exchange: the server limits those per IP, and one test binary
		// signs in more devices than the limit's burst.
		"TRUSTED_PROXIES=127.0.0.1",
	}
	s.cmd.Stdout, s.cmd.Stderr = logs, logs
	if err := s.cmd.Start(); err != nil {
		logs.Close()
		return fail(err)
	}
	go func() {
		_ = s.cmd.Wait()
		logs.Close()
		close(s.exited)
	}()
	if err := s.waitReady(60 * time.Second); err != nil {
		return fail(fmt.Errorf("%w\n%s", err, s.Logs()))
	}
	return s, "", nil
}

// backendDir is ../backend from this source file.
func backendDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "backend")
}

func withDatabase(pgURL, name string) (string, error) {
	u, err := url.Parse(pgURL)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

func (s *Server) exec(ctx context.Context, sql string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := pgconn.Connect(ctx, s.pgURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, sql).ReadAll()
	return err
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	return port, err
}

func (s *Server) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-s.exited:
			return errors.New("the server exited")
		default:
		}
		res, err := s.hc.Get(s.URL + "/readyz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("the server didn't get ready in time")
}

// Logs is what the server wrote so far, for a failing test's message.
func (s *Server) Logs() string {
	raw, _ := os.ReadFile(filepath.Join(s.dir, "server.log"))
	if len(raw) > 8<<10 {
		raw = raw[len(raw)-8<<10:]
	}
	return string(raw)
}

func (s *Server) stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(os.Interrupt)
		select {
		case <-s.exited:
		case <-time.After(10 * time.Second):
			_ = s.cmd.Process.Kill()
			<-s.exited
		}
	}
	if s.dbName != "" {
		if err := s.exec(context.Background(), "DROP DATABASE IF EXISTS "+s.dbName+" WITH (FORCE)"); err != nil {
			fmt.Fprintln(os.Stderr, "testserver: drop database:", err)
		}
	}
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ExpireJournal makes every change cursor of userID older than the
// server's journal, as SYNC_JOURNAL_RETENTION does after a month offline:
// their next /sync/changes answers 410 cursor_expired. Straight in the
// server's database, since no API does it.
func (s *Server) ExpireJournal(t testing.TB, userID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgconn.Connect(ctx, s.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	res := conn.ExecParams(ctx, `UPDATE sync_heads SET min_seq = tree_seq WHERE user_id = $1`, [][]byte{[]byte(userID)}, nil, nil, nil).Read()
	if res.Err != nil {
		t.Fatalf("expire the journal: %v", res.Err)
	}
	if res.CommandTag.RowsAffected() != 1 {
		t.Fatalf("expire the journal: no sync head for %s", userID)
	}
}

// User is an account on the test server.
type User struct {
	ID, Username, Password string
}

// CreateUser makes a user with a random name (prefix + suffix), each test
// its own, so their trees and change journals don't mix. quota is in
// bytes, 0 for none.
func (s *Server) CreateUser(t testing.TB, prefix string, quota int64) User {
	t.Helper()
	u := User{Username: prefix + "-" + randomHex(4), Password: "pw-" + randomHex(8)}
	body := map[string]any{"username": u.Username, "password": u.Password}
	if quota > 0 {
		body["quotaBytes"] = quota
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := s.JSON(http.MethodPost, "/api/v1/admin/users", s.admin(t), body, &out); err != nil {
		t.Fatalf("create user: %v", err)
	}
	u.ID = out.ID
	return u
}

func (s *Server) admin(t testing.TB) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.adminCookie == "" {
		s.adminCookie = s.Login(t, AdminUsername, AdminPassword)
	}
	return s.adminCookie
}

// Login signs in with a password and returns the Cookie header value of
// the session — what a browser has when it opens the consent page.
func (s *Server) Login(t testing.TB, username, password string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"username": username, "password": password})
	res, err := s.hc.Post(s.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login: %s", res.Status)
	}
	for _, c := range res.Cookies() {
		if c.Name == "session" || c.Name == "__Host-session" {
			return c.Name + "=" + c.Value
		}
	}
	t.Fatal("login: no session cookie")
	return ""
}

// Session is a password session of u, one per user for the whole run:
// the server rate-limits password sign-ins, and a test making several
// devices would hit the limit.
func (s *Server) Session(t testing.TB, u User) string {
	t.Helper()
	s.mu.Lock()
	cookie, ok := s.sessions[u.Username]
	s.mu.Unlock()
	if ok {
		return cookie
	}
	cookie = s.Login(t, u.Username, u.Password)
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]string{}
	}
	s.sessions[u.Username] = cookie
	s.mu.Unlock()
	return cookie
}

// ErrNoDeviceSignIn: the server has no device sign-in (E12-2) yet.
var ErrNoDeviceSignIn = errors.New("testserver: the server has no POST /api/v1/devices/authorize")

// Authorize does what the consent page does when the user clicks
// «Разрешить» (POST /api/v1/devices/authorize with the browser's
// session) and returns the redirect it answers: the client's loopback
// URL with ?code=&state=. challenge is the client's PKCE S256 challenge.
func (s *Server) Authorize(cookie, redirectURI, challenge, state, name string) (string, error) {
	var out struct {
		Redirect string `json:"redirect"`
	}
	err := s.JSON(http.MethodPost, "/api/v1/devices/authorize", cookie, map[string]string{
		"redirectUri": redirectURI, "codeChallenge": challenge, "codeChallengeMethod": "S256",
		"state": state, "name": name, "platform": platform(), "clientVersion": "0.0.0-test",
	}, &out)
	var se *StatusError
	if errors.As(err, &se) && (se.Status == http.StatusNotFound || se.Status == http.StatusMethodNotAllowed) {
		return "", ErrNoDeviceSignIn
	}
	return out.Redirect, err
}

// DeviceToken signs user in as a device the way the desktop client does,
// over HTTP only: password session, consent, code exchange. It skips t on
// a server without device sign-in.
func (s *Server) DeviceToken(t testing.TB, u User) string {
	t.Helper()
	verifierRaw := make([]byte, 32)
	_, _ = rand.Read(verifierRaw)
	verifier := base64.RawURLEncoding.EncodeToString(verifierRaw)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	const redirectURI = "http://127.0.0.1:50000/cb"
	redirect, err := s.Authorize(s.Session(t, u), redirectURI, challenge, "st", "test device")
	if errors.Is(err, ErrNoDeviceSignIn) {
		t.Skip("E12-2 isn't on the server yet: " + err.Error())
	}
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	ru, err := url.Parse(redirect)
	if err != nil || !strings.HasPrefix(redirect, redirectURI+"?") || ru.Query().Get("code") == "" {
		t.Fatalf("authorize: unexpected redirect")
	}
	var out struct {
		Token string `json:"token"`
	}
	fwd := http.Header{"X-Forwarded-For": {fmt.Sprintf("10.%d.%d.%d", verifierRaw[0], verifierRaw[1], verifierRaw[2])}}
	if err := s.do(http.MethodPost, "/api/v1/devices/token", "", fwd, map[string]string{
		"code": ru.Query().Get("code"), "codeVerifier": verifier, "redirectUri": redirectURI,
	}, &out); err != nil {
		t.Fatalf("token: %v", err)
	}
	if out.Token == "" {
		t.Fatal("token: empty")
	}
	return out.Token
}

func platform() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	}
	return "linux"
}

// StatusError is a non-2xx answer to JSON.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Body) }

// JSON sends in as JSON with the session cookie (if any) and decodes the
// answer into out (if any).
func (s *Server) JSON(method, path, cookie string, in, out any) error {
	return s.do(method, path, cookie, nil, in, out)
}

func (s *Server) do(method, path, cookie string, header http.Header, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.URL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &StatusError{Status: res.StatusCode, Body: string(raw)}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}
