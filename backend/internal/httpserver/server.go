// Package httpserver builds the public HTTP handler: routes plus the
// middleware every request passes through.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"time"

	"github.com/do-service/cloud/backend/internal/admin"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/httpx"
	"github.com/do-service/cloud/backend/internal/nodes"
	"github.com/do-service/cloud/backend/internal/share"
	"github.com/do-service/cloud/backend/internal/sync"
	"github.com/do-service/cloud/backend/internal/upload"
	"github.com/do-service/cloud/backend/internal/version"
	"github.com/do-service/cloud/backend/internal/wopi"
)

// Pinger is what /readyz needs from the database; *pgxpool.Pool satisfies
// it, tests pass a fake.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the dependencies New wires into the router.
type Deps struct {
	DB Pinger
	// DataDir is config.DataDir; /readyz checks it is a writable directory.
	DataDir string
	// WebUI serves everything no other route claims (webui.Handler()).
	WebUI http.Handler
	// PublicOrigin is PUBLIC_BASE_URL's origin, trusted for cross-origin
	// checks: behind a proxy that rewrites Host, the browser's Origin no
	// longer matches the Host the app sees.
	PublicOrigin string
	// Auth serves /api/v1/auth/*; nil leaves those routes out (tests).
	Auth *auth.Handlers
	// Nodes serves /api/v1/nodes/*; nil leaves those routes out (tests).
	Nodes *nodes.Handlers
	// Uploads serves the tus endpoint /api/v1/uploads/; nil leaves it out.
	Uploads *upload.Handlers
	// Sharing serves /api/v1/directory and /api/v1/admin/groups; nil
	// leaves them out.
	Sharing *share.Handlers
	// Sync serves /api/v1/sync/* to sync clients; nil leaves it out.
	Sync *sync.Handlers
	// TrustedProxies are the reverse proxies trusted to name the client
	// in X-Forwarded-For.
	TrustedProxies []netip.Prefix
	// HSTS: browsers reach the app over https, and should stick to it.
	HSTS bool
	// EditorOrigin is the online editor's origin as the browser reaches
	// it, "" while unknown: the CSP lets the app frame and post to it.
	EditorOrigin func() string
	// Admin serves /api/v1/admin/users, audit and system; nil leaves them
	// out.
	Admin *admin.Handlers
	// WOPI serves /wopi and the editor API; nil when no editor is
	// configured.
	WOPI *wopi.Handlers
	// WebDAV builds the WebDAV handler for a mount prefix, behind which
	// come the user name and the path; nil leaves WebDAV off (tests).
	WebDAV func(prefix string) http.Handler
}

// webdavPrefixes are where users' trees are mounted, as
// {prefix}/{username}/. The Nextcloud-style alias costs nothing and lets
// clients that hardcode that path connect (architecture §8).
var webdavPrefixes = []string{"/dav/files", "/remote.php/dav/files"}

// readyTimeout bounds each /readyz probe so a hung database makes the probe
// fail fast instead of piling up probe requests.
const readyTimeout = 2 * time.Second

// New returns the public handler. /metrics is deliberately absent — it is
// served on the separate metrics listener (see cmd/server).
func New(deps Deps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", readyzHandler(deps.DB, deps.DataDir))

	mux.HandleFunc("GET /api/v1/version", handleVersion)
	if deps.Auth != nil {
		deps.Auth.Register(mux)
	}
	if deps.Nodes != nil {
		deps.Nodes.Register(mux)
	}
	if deps.Uploads != nil {
		deps.Uploads.Register(mux)
	}
	if deps.Sharing != nil {
		deps.Sharing.Register(mux)
	}
	if deps.Sync != nil {
		deps.Sync.Register(mux)
	}
	if deps.WOPI != nil {
		deps.WOPI.Register(mux)
	}
	if deps.Admin != nil {
		deps.Admin.Register(mux)
	}

	if deps.WebDAV != nil {
		for _, prefix := range webdavPrefixes {
			h := deps.WebDAV(prefix)
			// Both forms: the mux would answer the bare one with a
			// redirect, which WebDAV clients don't follow for PROPFIND.
			mux.Handle(prefix, h)
			mux.Handle(prefix+"/", h)
		}
	}

	mux.Handle("/", deps.WebUI)

	editorOrigin := deps.EditorOrigin
	if editorOrigin == nil {
		editorOrigin = func() string { return "" }
	}
	// Order matters: realIP and requestID are outermost because they are
	// the layers that replace *http.Request; instrument must see the same
	// request the mux sets Pattern on. recoverPanic sits inside instrument
	// so a panic is logged and counted as the 500 it becomes.
	return realIP(deps.TrustedProxies, requestID(instrument(recoverPanic(securityHeaders(deps.HSTS, editorOrigin, crossOrigin(deps.PublicOrigin, mux))))))
}

// crossOrigin rejects state-changing browser requests from other origins:
// the CSRF defence for the session cookie, which SameSite=Lax alone doesn't
// give (sibling subdomains are same-site). Browsers mark every request with
// Sec-Fetch-Site or Origin; WebDAV and tus clients and the desktop sync
// client (a Bearer device token) send neither and pass, which is safe —
// they authenticate with credentials of their own, not a cookie a
// third-party page could make the browser attach: a page can only send a
// bearer token it already knows.
func crossOrigin(publicOrigin string, next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	if publicOrigin != "" {
		if err := cop.AddTrustedOrigin(publicOrigin); err != nil {
			// config.Parse has already validated PUBLIC_BASE_URL.
			panic(fmt.Sprintf("trusted origin %q: %v", publicOrigin, err))
		}
	}
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeCrossOrigin, "cross-origin request refused")
	}))
	return cop.Handler(next)
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleVersion(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"version": version.Version})
}

// readyzHandler reports whether this instance can serve traffic: the
// database answers and the storage root is writable. Unlike /healthz, a
// failure here should take the instance out of the load balancer, not
// restart it.
func readyzHandler(db Pinger, dataDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			slog.WarnContext(ctx, "readyz: database ping failed", "err", err)
			httpx.WriteError(w, http.StatusServiceUnavailable, httpx.CodeDatabaseNotReady, "database is not reachable")
			return
		}
		if err := checkWritableDir(dataDir); err != nil {
			slog.WarnContext(ctx, "readyz: data dir check failed", "err", err)
			httpx.WriteError(w, http.StatusServiceUnavailable, httpx.CodeDataDirNotReady, "data directory is missing or not writable")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// checkWritableDir actually creates a file rather than inspecting mode
// bits: permission bits miss read-only mounts, full disks and ACLs.
func checkWritableDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	f, err := os.CreateTemp(dir, ".readyz-*")
	if err != nil {
		return fmt.Errorf("create probe file: %w", err)
	}
	name := f.Name()
	return errors.Join(f.Close(), os.Remove(name))
}
