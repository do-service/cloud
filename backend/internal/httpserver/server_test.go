package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/do-service/cloud/backend/internal/httpx"
	"github.com/do-service/cloud/backend/internal/version"
	"github.com/do-service/cloud/backend/internal/webui"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func newTestServer(t *testing.T, db Pinger, dataDir string) http.Handler {
	t.Helper()
	return New(Deps{
		DB:      db,
		DataDir: dataDir,
		WebUI:   webui.New(fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}),
	})
}

func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestHealthz(t *testing.T) {
	// A broken DB must not fail liveness — restarting wouldn't fix it.
	h := newTestServer(t, fakePinger{err: errors.New("down")}, "/nonexistent")
	rec := do(h, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := decode[map[string]string](t, rec); body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
}

func TestReadyz(t *testing.T) {
	dataDir := t.TempDir()
	notADir := filepath.Join(dataDir, "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	readOnly := filepath.Join(dataDir, "ro")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		db         Pinger
		dir        string
		wantStatus int
		wantCode   string
	}{
		{"ready", fakePinger{}, dataDir, http.StatusOK, ""},
		{"db down", fakePinger{err: errors.New("connection refused")}, dataDir, http.StatusServiceUnavailable, httpx.CodeDatabaseNotReady},
		{"data dir missing", fakePinger{}, filepath.Join(dataDir, "missing"), http.StatusServiceUnavailable, httpx.CodeDataDirNotReady},
		{"data dir is a file", fakePinger{}, notADir, http.StatusServiceUnavailable, httpx.CodeDataDirNotReady},
		{"data dir read-only", fakePinger{}, readOnly, http.StatusServiceUnavailable, httpx.CodeDataDirNotReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dir == readOnly && os.Geteuid() == 0 {
				t.Skip("root ignores directory permissions")
			}
			rec := do(newTestServer(t, tc.db, tc.dir), httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body)
			}
			if tc.wantCode == "" {
				return
			}
			if body := decode[httpx.ErrorBody](t, rec); body.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", body.Code, tc.wantCode)
			}
		})
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("probe files left behind in data dir: %v", entries)
	}
}

func TestVersion(t *testing.T) {
	rec := do(newTestServer(t, fakePinger{}, t.TempDir()), httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := decode[map[string]string](t, rec); body["version"] != version.Version {
		t.Fatalf("body = %v, want version %q", body, version.Version)
	}
}

func TestUnknownAPIRouteIsJSON404(t *testing.T) {
	rec := do(newTestServer(t, fakePinger{}, t.TempDir()), httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := decode[httpx.ErrorBody](t, rec); body.Code != httpx.CodeNotFound {
		t.Fatalf("code = %q", body.Code)
	}
}

func TestSPAFallbackThroughServer(t *testing.T) {
	rec := do(newTestServer(t, fakePinger{}, t.TempDir()), httptest.NewRequest(http.MethodGet, "/files/a/b", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "<!doctype html>" {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body)
	}
}

func TestMetricsNotOnPublicMux(t *testing.T) {
	rec := do(newTestServer(t, fakePinger{}, t.TempDir()), httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), "cloud_http_requests_total") {
		t.Fatal("/metrics is exposed on the public handler")
	}
}

func TestSecurityHeaders(t *testing.T) {
	rec := do(newTestServer(t, fakePinger{}, t.TempDir()), httptest.NewRequest(http.MethodGet, "/", nil))
	want := map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"Referrer-Policy":            "same-origin",
		"Cross-Origin-Opener-Policy": "same-origin",
		"Permissions-Policy":         "camera=(), microphone=(), geolocation=(), payment=(), usb=()",
		// Over plain http, HSTS would lock browsers out.
		"Strict-Transport-Security": "",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "script-src 'self';", "object-src 'none'", "frame-ancestors 'self'", "frame-src 'self';", "form-action 'self'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP %q lacks %q", csp, directive)
		}
	}
	if strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP lets scripts in: %q", csp)
	}

	// Over https, with an editor known: HSTS, and the editor may be framed
	// and posted to.
	editor := ""
	h := New(Deps{DB: fakePinger{}, DataDir: t.TempDir(), WebUI: webui.New(fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}),
		HSTS: true, EditorOrigin: func() string { return editor }})
	if csp := do(h, httptest.NewRequest(http.MethodGet, "/", nil)).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src 'self';") {
		t.Errorf("unknown editor in CSP: %q", csp)
	}
	editor = "https://office.example.com"
	rec = do(h, httptest.NewRequest(http.MethodGet, "/", nil))
	csp = rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-src 'self' https://office.example.com;") || !strings.HasSuffix(csp, "form-action 'self' https://office.example.com") {
		t.Errorf("editor not allowed: %q", csp)
	}
	if rec.Header().Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Errorf("no HSTS over https")
	}
}

func TestRealIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	var seen string
	h := realIP(trusted, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.RemoteAddr }))
	for _, tc := range []struct {
		name, peer string
		xff        []string
		want       string
	}{
		{"direct, no proxy", "203.0.113.9:4000", nil, "203.0.113.9:4000"},
		{"untrusted peer can't claim an address", "203.0.113.9:4000", []string{"198.51.100.1"}, "203.0.113.9:4000"},
		{"through the proxy", "10.1.2.3:5000", []string{"198.51.100.7"}, "198.51.100.7:5000"},
		{"through two of ours", "10.1.2.3:5000", []string{"198.51.100.7, 10.9.9.9"}, "198.51.100.7:5000"},
		// The client wrote the leftmost one; the proxy appended the real one.
		{"spoofed entry ignored", "10.1.2.3:5000", []string{"1.1.1.1", "198.51.100.7"}, "198.51.100.7:5000"},
		{"garbage stops the walk", "10.1.2.3:5000", []string{"1.1.1.1, nonsense, 10.2.2.2"}, "10.2.2.2:5000"},
		{"proxy without header", "[::1]:5000", nil, "[::1]:5000"},
		{"ipv6 client", "[::1]:5000", []string{"2001:db8::5"}, "[2001:db8::5]:5000"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = tc.peer
		for _, v := range tc.xff {
			req.Header.Add("X-Forwarded-For", v)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if seen != tc.want {
			t.Errorf("%s: RemoteAddr = %q, want %q", tc.name, seen, tc.want)
		}
	}
	// Trusting no one changes nothing.
	plain := realIP(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.RemoteAddr }))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.1.2.3:5000"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	plain.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "10.1.2.3:5000" {
		t.Errorf("with no trusted proxies: %q", seen)
	}
}

func TestRequestID(t *testing.T) {
	var seen string
	h := requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}))
	cases := []struct {
		name, incoming string
		wantEcho       bool
	}{
		{"accepts sane id", "abc-123_DEF.9", true},
		{"generates when missing", "", false},
		{"rejects spaces", "a b", false},
		{"rejects log injection", "x\" level=ERROR", false},
		{"rejects too long", strings.Repeat("a", 65), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.incoming != "" {
				req.Header.Set(requestIDHeader, tc.incoming)
			}
			rec := do(h, req)
			got := rec.Header().Get(requestIDHeader)
			if got != seen {
				t.Fatalf("header %q != context %q", got, seen)
			}
			if tc.wantEcho && got != tc.incoming {
				t.Fatalf("id = %q, want echoed %q", got, tc.incoming)
			}
			if !tc.wantEcho && (got == tc.incoming || len(got) != 32) {
				t.Fatalf("id = %q, want a fresh 32-char id", got)
			}
		})
	}
}

// captureLogs swaps the default slog logger for one writing JSON into a
// buffer, restoring it after the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		lines = append(lines, m)
	}
	return lines
}

func TestAccessLog(t *testing.T) {
	buf := captureLogs(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	req.Header.Set(requestIDHeader, "req-42")
	do(newTestServer(t, fakePinger{}, t.TempDir()), req)

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines: %s", len(lines), buf)
	}
	l := lines[0]
	if l["msg"] != "http request" || l["method"] != "GET" || l["route"] != "/api/v1/version" ||
		l["status"] != float64(200) || l["request_id"] != "req-42" || l["level"] != "INFO" {
		t.Fatalf("log line = %v", l)
	}
	if b, ok := l["bytes"].(float64); !ok || b <= 0 {
		t.Fatalf("bytes = %v", l["bytes"])
	}
	if _, ok := l["duration"]; !ok {
		t.Fatalf("no duration in %v", l)
	}
}

func TestAccessLog_ProbesAtDebug(t *testing.T) {
	buf := captureLogs(t)
	do(newTestServer(t, fakePinger{}, t.TempDir()), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	lines := logLines(t, buf)
	if len(lines) != 1 || lines[0]["level"] != "DEBUG" {
		t.Fatalf("lines = %v", lines)
	}
}

func TestRecoverPanic(t *testing.T) {
	buf := captureLogs(t)
	h := requestID(instrument(recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(requestIDHeader, "req-panic")
	rec := do(h, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := decode[httpx.ErrorBody](t, rec); body.Code != httpx.CodeInternal {
		t.Fatalf("code = %q", body.Code)
	}
	var panicLine, accessLine map[string]any
	for _, l := range logLines(t, buf) {
		switch l["msg"] {
		case "panic in handler":
			panicLine = l
		case "http request":
			accessLine = l
		}
	}
	stack, _ := panicLine["stack"].(string)
	if panicLine["panic"] != "boom" || panicLine["request_id"] != "req-panic" || !strings.Contains(stack, "TestRecoverPanic") {
		t.Fatalf("panic log = %v", panicLine)
	}
	if accessLine == nil || accessLine["status"] != float64(500) {
		t.Fatalf("access log = %v", accessLine)
	}
}

func TestRecoverPanic_AfterHeadersAborts(t *testing.T) {
	captureLogs(t)
	h := recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic("late")
	}))
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	do(h, httptest.NewRequest(http.MethodGet, "/", nil))
}

// counterValue reads cloud_http_requests_total for one label set from the
// default registry; 0 if that series doesn't exist yet.
func counterValue(t *testing.T, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "cloud_http_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelsMatch(m, labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func routeSeries(t *testing.T) map[string]bool {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	routes := map[string]bool{}
	for _, f := range families {
		if f.GetName() != "cloud_http_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "route" {
					routes[lp.GetValue()] = true
				}
			}
		}
	}
	return routes
}

func TestMetricsLabelledByPattern(t *testing.T) {
	h := newTestServer(t, fakePinger{err: errors.New("down")}, t.TempDir())
	versionOK := map[string]string{"method": "GET", "route": "/api/v1/version", "status": "2xx"}
	readyFail := map[string]string{"method": "GET", "route": "/readyz", "status": "5xx"}
	spaOK := map[string]string{"method": "GET", "route": "/", "status": "2xx"}
	beforeVersion, beforeReady, beforeSPA := counterValue(t, versionOK), counterValue(t, readyFail), counterValue(t, spaOK)

	do(h, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))
	do(h, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	do(h, httptest.NewRequest(http.MethodGet, "/files/user-1/secret-report.pdf", nil))
	do(h, httptest.NewRequest(http.MethodGet, "/files/user-2/other.docx", nil))

	if got := counterValue(t, versionOK) - beforeVersion; got != 1 {
		t.Errorf("version 2xx delta = %v, want 1", got)
	}
	if got := counterValue(t, readyFail) - beforeReady; got != 1 {
		t.Errorf("readyz 5xx delta = %v, want 1", got)
	}
	if got := counterValue(t, spaOK) - beforeSPA; got != 2 {
		t.Errorf("SPA catch-all delta = %v, want 2", got)
	}
	for route := range routeSeries(t) {
		if strings.Contains(route, "user-") {
			t.Errorf("raw path leaked into route label: %q", route)
		}
	}
}

func TestWebDAVMount(t *testing.T) {
	var prefixes []string
	h := New(Deps{
		DB:    fakePinger{},
		WebUI: webui.New(fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}),
		WebDAV: func(prefix string) http.Handler {
			prefixes = append(prefixes, prefix)
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Prefix", prefix)
				w.WriteHeader(http.StatusMultiStatus)
			})
		},
	})
	if len(prefixes) != len(webdavPrefixes) {
		t.Fatalf("WebDAV built for %v", prefixes)
	}
	for _, p := range webdavPrefixes {
		// The bare prefix must reach the handler, not a mux redirect.
		for _, path := range []string{p, p + "/", p + "/alice/docs/a.txt"} {
			rec := do(h, httptest.NewRequest("PROPFIND", path, nil))
			if rec.Code != http.StatusMultiStatus || rec.Header().Get("X-Prefix") != p {
				t.Errorf("PROPFIND %s: status %d, prefix %q", path, rec.Code, rec.Header().Get("X-Prefix"))
			}
		}
	}
}

func TestWebDAVOffByDefault(t *testing.T) {
	h := newTestServer(t, fakePinger{}, t.TempDir())
	rec := do(h, httptest.NewRequest("PROPFIND", "/dav/files/alice/", nil))
	if rec.Code == http.StatusMultiStatus {
		t.Fatal("WebDAV answered without being enabled")
	}
}

func TestCrossOriginProtection(t *testing.T) {
	h := New(Deps{
		DB:           fakePinger{},
		WebUI:        webui.New(fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}),
		PublicOrigin: "https://cloud.example.com",
	})
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		blocked bool
	}{
		{"cross-site post", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		// Sibling subdomains are same-site, which SameSite=Lax lets through.
		{"same-site post", "POST", map[string]string{"Sec-Fetch-Site": "same-site"}, true},
		{"foreign origin, old browser", "POST", map[string]string{"Origin": "https://evil.example"}, true},
		{"same-origin post", "POST", map[string]string{"Sec-Fetch-Site": "same-origin"}, false},
		// Behind a proxy that rewrites Host the Origin still matches
		// PUBLIC_BASE_URL.
		{"trusted public origin", "POST", map[string]string{"Origin": "https://cloud.example.com"}, false},
		// WebDAV and tus clients send neither header.
		{"non-browser client", "PUT", nil, false},
		{"cross-site get", "GET", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/v1/version", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := do(h, req)
			blocked := rec.Code == http.StatusForbidden
			if blocked != tc.blocked {
				t.Fatalf("status %d, blocked %v, want %v", rec.Code, blocked, tc.blocked)
			}
			if blocked {
				if body := decode[httpx.ErrorBody](t, rec); body.Code != httpx.CodeCrossOrigin {
					t.Fatalf("code %q", body.Code)
				}
			}
		})
	}
}
