package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/httpx"
)

const (
	// Over https the cookie carries the __Host- prefix: the browser then
	// only accepts it Secure, for Path=/ and without Domain, so a sibling
	// subdomain can't plant or shadow it. Browsers refuse the prefix
	// without Secure, hence the plain name over http (local dev).
	hostCookieName  = "__Host-session"
	plainCookieName = "session"
)

type userKey struct{}

// UserFromContext returns the request's authenticated user.
func UserFromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey{}).(User)
	return u, ok
}

// Cookies sets and reads the session cookie the same way everywhere.
type Cookies struct {
	// Secure follows PUBLIC_BASE_URL's scheme, not the request's: behind
	// a TLS-terminating proxy the app itself only ever sees plain http.
	Secure bool
}

func (c Cookies) name() string {
	if c.Secure {
		return hostCookieName
	}
	return plainCookieName
}

func (c Cookies) Set(w http.ResponseWriter, raw string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     c.name(),
		Value:    raw,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   c.Secure,
		// Lax, not Strict: Strict would drop the cookie when the user
		// follows a link to the app from mail or chat and show them the
		// login page despite a live session. Cross-site writes are stopped
		// by CrossOriginProtection instead.
		SameSite: http.SameSiteLaxMode,
	})
}

func (c Cookies) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: c.name(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: c.Secure, SameSite: http.SameSiteLaxMode})
}

func (c Cookies) read(r *http.Request) (string, bool) {
	ck, err := r.Cookie(c.name())
	if err != nil || ck.Value == "" {
		return "", false
	}
	return ck.Value, true
}

// Middleware attaches the request's user, if there is a live session or,
// with devices, a valid device token ("Authorization: Bearer cld_…"); it
// never rejects a missing or dead credential (see RequireUser), only a
// device reaching beyond its scope (deviceMayUse). Applied to API routes
// only: static assets carry the cookie too, and each would cost a query.
func Middleware(sessions *Sessions, devices *Devices, cookies Cookies) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A bearer token is the client's explicit choice of identity:
			// a cookie alongside it is not consulted, valid or not.
			if token, ok := bearerToken(r); ok {
				if devices == nil {
					next.ServeHTTP(w, r)
					return
				}
				serveDevice(w, r, devices, token, next)
				return
			}
			raw, ok := cookies.read(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			u, renewed, err := sessions.Lookup(r.Context(), raw)
			switch {
			case errors.Is(err, ErrNoSession):
				// Expired or revoked: drop the dead cookie so the browser
				// stops sending it.
				cookies.Clear(w)
				next.ServeHTTP(w, r)
				return
			case err != nil:
				slog.ErrorContext(r.Context(), "auth: session lookup failed", "err", err)
				httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
				return
			}
			if !renewed.IsZero() {
				cookies.Set(w, raw, renewed)
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
		})
	}
}

func serveDevice(w http.ResponseWriter, r *http.Request, devices *Devices, token string, next http.Handler) {
	u, dev, err := devices.Lookup(r.Context(), token, clientIP(r))
	switch {
	case errors.Is(err, ErrNoDevice):
		// Revoked, or the account blocked: anonymous, so RequireUser
		// answers 401 and the client knows to sign in again.
		next.ServeHTTP(w, r)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "auth: device lookup failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	}
	if !deviceMayUse(r) {
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeDeviceForbidden, "not available with a device token; use the web interface")
		return
	}
	ctx := context.WithValue(r.Context(), userKey{}, u)
	ctx = context.WithValue(ctx, deviceKey{}, dev)
	// Changes it makes are journaled as the device's, so its sync client
	// can tell its own writes from the rest.
	ctx = fs.WithDevice(ctx, dev.ID)
	next.ServeHTTP(w, r.WithContext(ctx))
}

// RequireUser answers 401 unless Middleware found a user.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := UserFromContext(r.Context()); !ok {
			httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "sign in required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP is the TCP peer. Behind a reverse proxy that is the proxy;
// trusting X-Forwarded-For needs a configured list of proxies (E10-2).
func clientIP(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

// ClientIP is the request's peer address, as rate limits key it.
func ClientIP(r *http.Request) netip.Addr { return clientIP(r) }
