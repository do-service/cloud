package auth

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/do-service/cloud/backend/internal/httpx"
	"github.com/do-service/cloud/backend/internal/ratelimit"
)

// NewBasicFailureLimiter limits failed Basic logins per IP: 20, then one
// every 3 s. Only failures count — a sync client authenticates on every
// request, and those must never run into a limit.
func NewBasicFailureLimiter() *ratelimit.Limiter {
	return ratelimit.New(3*time.Second, 20)
}

// BasicAuth authenticates non-browser clients (WebDAV) by username and
// app password. The account password isn't accepted: OIDC users have
// none, and Argon2 on every request would be a denial of service waiting
// to happen.
func BasicAuth(apps *AppPasswords, failures *ratelimit.Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			username, password, ok := r.BasicAuth()
			if !ok {
				challenge(w, "sign in with your username and an app password")
				return
			}
			ip := clientIP(r).String()
			if failures != nil {
				if blocked, retry := failures.Blocked(ip); blocked {
					w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
					httpx.WriteError(w, http.StatusTooManyRequests, httpx.CodeTooManyRequests, "too many failed sign-ins, retry later")
					return
				}
			}
			u, err := apps.Authenticate(r.Context(), username, password)
			if errors.Is(err, ErrInvalidCredentials) {
				if failures != nil {
					failures.Allow(ip)
				}
				challenge(w, "invalid username or app password")
				return
			}
			if err != nil {
				slog.ErrorContext(r.Context(), "auth: basic", "err", err)
				httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
		})
	}
}

func challenge(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="Cloud", charset="UTF-8"`)
	httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, msg)
}
