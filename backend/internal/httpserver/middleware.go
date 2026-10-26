package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/do-service/cloud/backend/internal/httpx"
	"github.com/do-service/cloud/backend/internal/metrics"
)

const requestIDHeader = "X-Request-ID"

// validRequestID bounds what we accept from clients/proxies: the ID ends up
// in logs and response headers, so anything that could forge log fields or
// bloat them is replaced with a fresh one.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type requestIDKey struct{}

// RequestIDFromContext returns the current request's ID, or "" outside a
// request.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// requestID reuses an incoming X-Request-ID (set by the reverse proxy, so
// proxy and app logs correlate) when it looks sane, else generates one.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func newRequestID() string {
	var b [16]byte
	// crypto/rand.Read never returns an error (it crashes the program
	// instead on an irrecoverable failure).
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// quietRoutes are polled by orchestrators every few seconds; logging them at
// info would drown real traffic.
var quietRoutes = map[string]bool{"GET /healthz": true, "GET /readyz": true}

// instrument writes the access log line and HTTP metrics once the handler
// returns. It reads r.Pattern after next has run: ServeMux fills it in on
// this same *http.Request, so no layer between here and the mux may
// replace the request.
func instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := newResponseRecorder(w)
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start)

		metrics.ObserveHTTP(r.Method, r.Pattern, rec.status, elapsed)

		level := slog.LevelInfo
		if quietRoutes[r.Pattern] {
			level = slog.LevelDebug
		}
		// The raw path is not logged: it carries user file names.
		slog.LogAttrs(r.Context(), level, "http request",
			slog.String("method", r.Method),
			slog.String("route", metrics.RouteLabel(r.Pattern)),
			slog.Int("status", rec.status),
			slog.Int64("bytes", rec.bytes),
			slog.Duration("duration", elapsed),
			slog.String("request_id", RequestIDFromContext(r.Context())),
		)
	})
}

// recoverPanic turns a handler panic into a logged stack trace and a 500
// JSON error instead of net/http's default of dropping the connection.
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec, ok := w.(*responseRecorder)
		if !ok {
			rec = newResponseRecorder(w)
		}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// ErrAbortHandler is net/http's sanctioned way to abort a
			// response; let it do its job.
			if v == http.ErrAbortHandler {
				panic(v)
			}
			slog.ErrorContext(r.Context(), "panic in handler",
				"panic", v,
				"request_id", RequestIDFromContext(r.Context()),
				"stack", string(debug.Stack()),
			)
			if rec.wroteHeader {
				// Too late for a clean 500: abort so the client sees a
				// truncated response rather than a "successful" partial one.
				panic(http.ErrAbortHandler)
			}
			httpx.WriteError(rec, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		}()
		next.ServeHTTP(rec, r)
	})
}

// securityHeaders sets the headers every response gets (R19). Handlers
// may override them: a download replaces the CSP with its own sandbox.
//
// The CSP lets scripts come from the app's own files only — the bundle has
// no inline script and no eval — which is what stops an injected script.
// Styles may be inline: Mantine sets them as attributes. The online
// editor's origin, once known, may be framed and posted to (the token goes
// to it in a form). hsts: the browser reaches the app over https.
func securityHeaders(hsts bool, editorOrigin func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		// Share links carry their secret token in the URL; same-origin keeps
		// it from leaking to third-party sites via the Referer header.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", contentSecurityPolicy(editorOrigin()))
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func contentSecurityPolicy(editor string) string {
	frames := "'self'"
	if editor != "" {
		frames += " " + editor
	}
	return "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
		"font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'self'; " +
		"frame-src " + frames + "; form-action " + frames
}

// realIP makes RemoteAddr the client's address when the request came
// through one of the trusted reverse proxies, so that the audit log, the
// rate limits and the sessions see the client rather than the proxy. Each
// proxy appends to X-Forwarded-For the address it got the request from:
// read from the right, the first address that isn't a trusted proxy is the
// client; whatever stands left of it the client may have made up.
func realIP(trusted []netip.Prefix, next http.Handler) http.Handler {
	if len(trusted) == 0 {
		return next
	}
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil || !isTrusted(peer.Addr().Unmap()) {
			next.ServeHTTP(w, r)
			return
		}
		client := peer.Addr().Unmap()
		var hops []string
		for _, v := range r.Header.Values("X-Forwarded-For") {
			hops = append(hops, strings.Split(v, ",")...)
		}
		for i := len(hops) - 1; i >= 0 && isTrusted(client); i-- {
			a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
			if err != nil {
				// Garbage in the chain: trust nothing beyond it.
				break
			}
			client = a.Unmap()
		}
		r = r.WithContext(r.Context())
		r.RemoteAddr = netip.AddrPortFrom(client, peer.Port()).String()
		next.ServeHTTP(w, r)
	})
}

// responseRecorder captures status and body size for logging/metrics.
// Unwrap lets http.ResponseController reach the underlying writer (per-route
// deadlines, Flush), and ReadFrom keeps the sendfile fast path for file
// downloads, which would otherwise be lost behind the wrapper.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func newResponseRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (rec *responseRecorder) WriteHeader(code int) {
	if rec.wroteHeader {
		return
	}
	// 1xx informational responses may precede the real one.
	if code >= 200 {
		rec.status = code
		rec.wroteHeader = true
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *responseRecorder) Write(b []byte) (int, error) {
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += int64(n)
	return n, err
}

func (rec *responseRecorder) ReadFrom(src io.Reader) (int64, error) {
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	var n int64
	var err error
	if rf, ok := rec.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(src)
	} else {
		// Hide our own ReadFrom from io.Copy to avoid recursing into it.
		n, err = io.Copy(struct{ io.Writer }{rec.ResponseWriter}, src)
	}
	rec.bytes += n
	return n, err
}

func (rec *responseRecorder) Flush() {
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(rec.ResponseWriter).Flush()
}

func (rec *responseRecorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}
