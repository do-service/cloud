// Package app is what the desktop window runs on besides Wails itself:
// the connections and their engines (Manager), the window's own API
// (/app/v1, Handler), the proxy of /api to the server, and the tray's
// state. It doesn't import Wails, so it builds and tests with
// CGO_ENABLED=0.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strings"

	"github.com/do-service/cloud/desktop/internal/api"
)

// Upstream is where the window's /api goes: the server of the connection
// it shows, and that connection's device token.
type Upstream struct {
	Server *url.URL
	Token  api.TokenSource
}

// ErrNoUpstream: no connection is shown, so /api has nowhere to go.
var ErrNoUpstream = errors.New("app: no connection selected")

type upstreamKey struct{}

// NewProxy forwards the window's /api/* to the server, so the web
// frontend's api.ts, files.ts and preview.ts work unchanged with relative
// URLs (docs/desktop-client.md). The proxy, not the WebView, is the
// server's client: it adds the device token, which the page never sees.
// pick tells the upstream per request: the connection the window shows.
func NewProxy(pick func() (Upstream, error)) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			up, _ := pr.In.Context().Value(upstreamKey{}).(upstream)
			pr.SetURL(up.server)
			h := pr.Out.Header
			// The page's origin is wails://localhost (or the dev server):
			// passed on, the server's CrossOriginProtection would refuse
			// every write as cross-origin. The request is ours now.
			for _, name := range []string{"Cookie", "Authorization", "Origin", "Referer"} {
				h.Del(name)
			}
			for name := range h {
				if strings.HasPrefix(name, "Sec-Fetch-") {
					h.Del(name)
				}
			}
			h.Set("User-Agent", "cloud-desktop/"+api.Version+" ("+runtime.GOOS+")")
			h.Set("Authorization", "Bearer "+up.token)
		},
		ModifyResponse: func(res *http.Response) error {
			// Nothing of the server's sessions belongs in the WebView.
			res.Header.Del("Set-Cookie")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			slog.WarnContext(r.Context(), "proxy: server unreachable", "path", r.URL.Path, "err", err)
			writeError(w, http.StatusBadGateway, "network", "cloud server unreachable")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up, err := pick()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "network", "no connection selected")
			return
		}
		tok, err := up.Token.Token(r.Context())
		if err != nil {
			// No token stored: as good as signed out.
			slog.WarnContext(r.Context(), "proxy: no device token", "server", up.Server.Host, "err", err)
			writeError(w, http.StatusUnauthorized, "unauthorized", "not signed in")
			return
		}
		ctx := context.WithValue(r.Context(), upstreamKey{}, upstream{server: up.Server, token: tok})
		rp.ServeHTTP(w, r.WithContext(ctx))
	})
}

type upstream struct {
	server *url.URL
	token  string
}

// writeError answers in the server's {"code","message"} shape, so the
// page's errorText shows the same text as for the server's own errors.
// "network" is the web client's code for "no answer from the server",
// which is what a failed hop through the proxy is to the page.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
