// Package webui serves the single-page frontend from the binary itself.
package webui

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/do-service/cloud/backend/internal/httpx"
)

// dist is filled by the build pipeline, which copies frontend/dist here
// before `go build`. Only dist/.gitkeep is committed, so the embed compiles
// in a backend-only checkout; Handler then answers with frontend_not_built.
//
//go:embed all:dist
var dist embed.FS

// reservedPrefixes belong to backend subsystems. Unknown paths under them
// must 404, not get the SPA's index.html — a WebDAV client or API caller
// receiving a 200 HTML page would misread it as success.
var reservedPrefixes = []string{"/api", "/dav", "/wopi", "/healthz", "/readyz"}

// Handler serves the embedded frontend build.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// fs.Sub only fails on an invalid path literal, a programming error.
		panic(err)
	}
	return New(sub)
}

// New serves the SPA from fsys (the root of a frontend build): existing
// files as-is, any other non-reserved path as index.html so client-side
// routing works on reload and deep links.
func New(fsys fs.FS) http.Handler {
	return &handler{fsys: fsys}
}

type handler struct {
	fsys fs.FS
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isReserved(r.URL.Path) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such endpoint")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		httpx.WriteError(w, http.StatusMethodNotAllowed, httpx.CodeMethodNotAllowed, "the web UI only answers GET and HEAD")
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "index.html" {
		h.serveIndex(w, r)
		return
	}
	if info, err := fs.Stat(h.fsys, name); err == nil && !info.IsDir() {
		if strings.HasPrefix(name, "assets/") {
			// Vite puts content hashes in every filename under assets/, so a
			// changed file always gets a new URL and the old one can be
			// cached forever.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.ServeFileFS(w, r, h.fsys, name)
		return
	}
	if strings.HasPrefix(name, "assets/") {
		// A missing hashed asset (stale tab after a deploy) must not get
		// index.html: the browser would try to run HTML as JS/CSS.
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such asset")
		return
	}
	h.serveIndex(w, r)
}

func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	index, err := fs.ReadFile(h.fsys, "index.html")
	if errors.Is(err, fs.ErrNotExist) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeFrontendNotBuilt,
			"frontend is not embedded in this build; build frontend/ and copy its dist into internal/webui/dist, or use the Vite dev server")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "cannot read index.html")
		return
	}
	// index.html references the current hashed assets, so it must be
	// revalidated on every load or users keep running the old build.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(index)
	}
}

func isReserved(urlPath string) bool {
	// A public link's page (/s/{token}) is the SPA's; its API under
	// /s/{token}/api is the backend's.
	if rest, ok := strings.CutPrefix(urlPath, "/s/"); ok {
		_, sub, _ := strings.Cut(rest, "/")
		return sub == "api" || strings.HasPrefix(sub, "api/")
	}
	for _, prefix := range reservedPrefixes {
		if urlPath == prefix || strings.HasPrefix(urlPath, prefix+"/") {
			return true
		}
	}
	return false
}
