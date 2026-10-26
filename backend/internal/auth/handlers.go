package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/httpx"
	"github.com/do-service/cloud/backend/internal/ratelimit"
)

// Handlers serves /api/v1/auth/*, the account's /api/v1/me/app-passwords
// and /api/v1/me/devices, and the desktop sign-in /api/v1/devices/*.
type Handlers struct {
	Sessions *Sessions
	Cookies  Cookies
	// Users enables password login; nil leaves the route out.
	Users *Users
	// Login attempts are limited per client IP (one host guessing many
	// accounts) and per username (many hosts guessing one account).
	LoginPerIP   *ratelimit.Limiter
	LoginPerUser *ratelimit.Limiter
	// AppPasswords enables /api/v1/me/app-passwords; nil leaves it out.
	AppPasswords *AppPasswords
	// Devices enables device tokens and /api/v1/me/devices; nil leaves
	// them out.
	Devices *Devices
	// DeviceTokenPerIP limits code exchanges per client IP; nil doesn't.
	DeviceTokenPerIP *ratelimit.Limiter
	// OIDC enables sign-in through its providers; nil leaves the routes
	// out. Needs Users, which keeps the accounts.
	OIDC *OIDC
	// OnUserCreated sets up an account made on a first OIDC sign-in
	// (its root folder).
	OnUserCreated func(ctx context.Context, u User) error
	// Audit records sign-ins, app passwords and devices; nil records
	// nothing.
	Audit *audit.Log
}

// Actor is the request's user as the audit log names them.
func Actor(ctx context.Context) audit.Actor {
	u, _ := UserFromContext(ctx)
	return audit.Actor{ID: u.ID, Name: u.Username}
}

// NewLoginLimiters are the defaults: an IP gets 20 attempts, then one every
// 3 s; a username 5, then one every 30 s — enough for a person mistyping,
// far too slow for guessing.
func NewLoginLimiters() (perIP, perUser *ratelimit.Limiter) {
	return ratelimit.New(3*time.Second, 20), ratelimit.New(30*time.Second, 5)
}

// NewDeviceTokenLimiter is the default for DeviceTokenPerIP: a client
// exchanges one code per sign-in, so 10 at once, then one every 6 s, only
// ever slows down a script. Codes are 256 random bits and die on the first
// try anyway; the limit keeps the endpoint from being a free DB load.
func NewDeviceTokenLimiter() *ratelimit.Limiter {
	return ratelimit.New(6*time.Second, 10)
}

// maxLoginBody fits any username plus a maxPassword password as JSON.
const maxLoginBody = 4 << 10

// maxAppPasswordBody fits a maxAppName name as JSON.
const maxAppPasswordBody = 1 << 10

// maxDeviceAuthBody fits a device name, a state and a loopback URI, all
// at their limits and escaped, as JSON.
const maxDeviceAuthBody = 4 << 10

// Register mounts the auth routes on mux.
func (h *Handlers) Register(mux *http.ServeMux) {
	withUser := Middleware(h.Sessions, h.Devices, h.Cookies)
	signedIn := func(f http.HandlerFunc) http.Handler { return withUser(RequireUser(f)) }
	mux.Handle("GET /api/v1/auth/me", withUser(RequireUser(http.HandlerFunc(h.me))))
	// With the session's user, if any, for the audit log.
	mux.Handle("POST /api/v1/auth/logout", withUser(http.HandlerFunc(h.logout)))
	mux.Handle("GET /api/v1/auth/providers", http.HandlerFunc(h.providers))
	if h.Users != nil {
		mux.Handle("POST /api/v1/auth/login", httpx.LimitBody(maxLoginBody)(http.HandlerFunc(h.login)))
	}
	if h.AppPasswords != nil {
		mux.Handle("GET /api/v1/me/app-passwords", signedIn(h.listAppPasswords))
		mux.Handle("POST /api/v1/me/app-passwords", httpx.LimitBody(maxAppPasswordBody)(signedIn(h.createAppPassword)))
		mux.Handle("DELETE /api/v1/me/app-passwords/{id}", signedIn(h.deleteAppPassword))
	}
	if h.Devices != nil {
		// Listing and revoking are for the web settings; a device itself
		// may only sign itself out (deviceMayUse, in Middleware).
		mux.Handle("GET /api/v1/me/devices", signedIn(h.listDevices))
		mux.Handle("DELETE /api/v1/me/devices/current", signedIn(h.signOutDevice))
		mux.Handle("DELETE /api/v1/me/devices/{id}", signedIn(h.revokeDevice))
		// The consent page's: a session only (deviceMayUse turns a device
		// away), and CrossOriginProtection guards it like any write.
		mux.Handle("POST /api/v1/devices/authorize", httpx.LimitBody(maxDeviceAuthBody)(signedIn(h.authorizeDevice)))
		// The client's, with no credential but the code and its verifier:
		// nothing here reads a cookie, so a cross-site page posting to it
		// gains nothing it doesn't already hold. CrossOriginProtection
		// still applies and passes the client, which sends no Origin.
		mux.Handle("POST /api/v1/devices/token", httpx.LimitBody(maxDeviceAuthBody)(http.HandlerFunc(h.deviceToken)))
	}
	if h.OIDC != nil {
		mux.Handle("GET /api/v1/auth/oidc/{slug}/start", http.HandlerFunc(h.oidcStart))
		mux.Handle("GET /api/v1/auth/oidc/{slug}/callback", http.HandlerFunc(h.oidcCallback))
	}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Handlers) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "expected {\"username\", \"password\"}")
		return
	}
	// Both buckets are checked before any hashing, so a flood costs us a
	// map lookup per request, not 64 MiB of Argon2.
	ip := clientIP(r)
	for _, check := range []struct {
		l   *ratelimit.Limiter
		key string
	}{{h.LoginPerIP, ip.String()}, {h.LoginPerUser, strings.ToLower(req.Username)}} {
		if check.l == nil {
			continue
		}
		if ok, retry := check.l.Allow(check.key); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
			httpx.WriteError(w, http.StatusTooManyRequests, httpx.CodeTooManyRequests, "too many login attempts, retry later")
			return
		}
	}

	u, err := h.Users.Authenticate(r.Context(), req.Username, req.Password)
	if errors.Is(err, ErrInvalidCredentials) {
		h.Audit.Record(r, audit.Event{Actor: audit.Actor{Name: req.Username}, Action: audit.LoginFailed, Details: map[string]any{"method": "password"}})
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeInvalidCredentials, "invalid username or password")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "auth: login", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	}
	if err := h.startSession(w, r, u); err != nil {
		slog.ErrorContext(r.Context(), "auth: login", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	}
	h.Audit.Record(r, audit.Event{Actor: audit.Actor{ID: u.ID, Name: u.Username}, Action: audit.Login, Details: map[string]any{"method": "password"}})
	httpx.WriteJSON(w, http.StatusOK, u)
}

// startSession signs u in on this browser. A fresh token on every login,
// and the session the browser came with (if any) ended: a token planted
// before login can't be ridden after it (session fixation).
func (h *Handlers) startSession(w http.ResponseWriter, r *http.Request, u User) error {
	if old, ok := h.Cookies.read(r); ok {
		if err := h.Sessions.Delete(r.Context(), old); err != nil {
			slog.WarnContext(r.Context(), "auth: drop previous session", "err", err)
		}
	}
	raw, expires, err := h.Sessions.Create(r.Context(), u.ID, clientIP(r), r.UserAgent())
	if err != nil {
		return err
	}
	h.Cookies.Set(w, raw, expires)
	return nil
}

func (h *Handlers) me(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	httpx.WriteJSON(w, http.StatusOK, u)
}

// logout works without a valid session too: the outcome the caller wants
// — no session behind this cookie — holds either way.
func (h *Handlers) logout(w http.ResponseWriter, r *http.Request) {
	if raw, ok := h.Cookies.read(r); ok {
		if err := h.Sessions.Delete(r.Context(), raw); err != nil {
			slog.ErrorContext(r.Context(), "auth: logout", "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
			return
		}
	}
	if _, ok := UserFromContext(r.Context()); ok {
		h.Audit.Record(r, audit.Event{Actor: Actor(r.Context()), Action: audit.Logout})
	}
	h.Cookies.Clear(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) listAppPasswords(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	list, err := h.AppPasswords.List(r.Context(), u.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "auth: list app passwords", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	}
	if list == nil {
		list = []AppPassword{}
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

type createdAppPassword struct {
	AppPassword
	// Password is in this response only; it can't be read back later.
	Password string `json:"password"`
}

func (h *Handlers) createAppPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "expected {\"name\"}")
		return
	}
	u, _ := UserFromContext(r.Context())
	p, password, err := h.AppPasswords.Create(r.Context(), u.ID, req.Name)
	switch {
	case errors.Is(err, ErrInvalidName):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "name must be 1 to 100 characters")
		return
	case errors.Is(err, ErrTooManyAppPassword):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeAppPasswordLimit, "app password limit reached; revoke one first")
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "auth: create app password", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	}
	slog.InfoContext(r.Context(), "app password created", "username", u.Username, "name", p.Name)
	h.Audit.Record(r, audit.Event{Actor: Actor(r.Context()), Action: audit.AppPasswordAdd, TargetID: p.ID, TargetName: p.Name})
	// The password must not linger in caches or history.
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, createdAppPassword{AppPassword: p, Password: password})
}

func (h *Handlers) deleteAppPassword(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	err := h.AppPasswords.Delete(r.Context(), u.ID, r.PathValue("id"))
	if errors.Is(err, ErrNoAppPassword) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such app password")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "auth: delete app password", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	}
	h.Audit.Record(r, audit.Event{Actor: Actor(r.Context()), Action: audit.AppPasswordDrop, TargetID: r.PathValue("id")})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) listDevices(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	list, err := h.Devices.List(r.Context(), u.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "auth: list devices", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	}
	if list == nil {
		list = []Device{}
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

func (h *Handlers) revokeDevice(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	dev, err := h.Devices.Delete(r.Context(), u.ID, r.PathValue("id"))
	if errors.Is(err, ErrNoDevice) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such device")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "auth: revoke device", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	}
	h.Audit.Record(r, audit.Event{Actor: Actor(r.Context()), Action: audit.DeviceRevoke, TargetID: dev.ID, TargetName: dev.Name})
	w.WriteHeader(http.StatusNoContent)
}

// signOutDevice is the client's "sign out": it revokes the token the
// request came with.
func (h *Handlers) signOutDevice(w http.ResponseWriter, r *http.Request) {
	cur, ok := DeviceFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "the request carries no device token")
		return
	}
	u, _ := UserFromContext(r.Context())
	dev, err := h.Devices.Delete(r.Context(), u.ID, cur.ID)
	if errors.Is(err, ErrNoDevice) {
		// Revoked from the web a moment ago: signed out either way.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "auth: sign out device", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	}
	h.Audit.Record(r, audit.Event{Actor: Actor(r.Context()), Action: audit.DeviceSignOut, TargetID: dev.ID, TargetName: dev.Name})
	w.WriteHeader(http.StatusNoContent)
}

type authorizeDeviceRequest struct {
	RedirectURI         string `json:"redirectUri"`
	CodeChallenge       string `json:"codeChallenge"`
	CodeChallengeMethod string `json:"codeChallengeMethod"`
	State               string `json:"state"`
	Name                string `json:"name"`
	Platform            string `json:"platform"`
	ClientVersion       string `json:"clientVersion"`
	// Deny is the user's refusal: the answer is the client's redirect
	// with access_denied, built by the same rules as the code's.
	Deny bool `json:"deny"`
}

type redirectResponse struct {
	Redirect string `json:"redirect"`
}

// authorizeDevice is the consent page's "Разрешить" (or, with deny,
// "Отказать"): it answers where to send the browser, the client's
// loopback redirect with a one-time code.
func (h *Handlers) authorizeDevice(w http.ResponseWriter, r *http.Request) {
	var req authorizeDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "expected {\"redirectUri\", \"codeChallenge\", \"codeChallengeMethod\", \"state\", \"name\", \"platform\", \"clientVersion\"}")
		return
	}
	u, _ := UserFromContext(r.Context())
	var redirect string
	var err error
	if req.Deny {
		redirect, err = DenyRedirect(req.RedirectURI, req.State)
	} else {
		redirect, err = h.Devices.Authorize(r.Context(), u.ID, DeviceGrant{
			RedirectURI: req.RedirectURI, CodeChallenge: req.CodeChallenge, State: req.State,
			Name: req.Name, Platform: req.Platform, ClientVersion: req.ClientVersion,
		}, req.CodeChallengeMethod)
	}
	switch {
	case errors.Is(err, ErrInvalidRedirect), errors.Is(err, ErrInvalidChallenge), errors.Is(err, ErrInvalidState),
		errors.Is(err, ErrInvalidName), errors.Is(err, ErrInvalidPlatform), errors.Is(err, ErrInvalidVersion):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, err.Error())
		return
	case errors.Is(err, ErrTooManyDevices):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeDeviceLimit, "device limit reached; revoke one first")
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "auth: authorize device", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	}
	// The code is in the answer.
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, redirectResponse{Redirect: redirect})
}

type deviceTokenRequest struct {
	Code         string `json:"code"`
	CodeVerifier string `json:"codeVerifier"`
	RedirectURI  string `json:"redirectUri"`
}

type deviceTokenResponse struct {
	// Token is in this response only; it can't be read back later.
	Token  string `json:"token"`
	Device Device `json:"device"`
	User   User   `json:"user"`
}

// deviceToken is the client's half: the code from the redirect and the
// verifier only it knows, for a device token.
func (h *Handlers) deviceToken(w http.ResponseWriter, r *http.Request) {
	if h.DeviceTokenPerIP != nil {
		if ok, retry := h.DeviceTokenPerIP.Allow(clientIP(r).String()); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
			httpx.WriteError(w, http.StatusTooManyRequests, httpx.CodeTooManyRequests, "too many attempts, retry later")
			return
		}
	}
	var req deviceTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "expected {\"code\", \"codeVerifier\", \"redirectUri\"}")
		return
	}
	u, dev, token, err := h.Devices.Exchange(r.Context(), req.Code, req.CodeVerifier, req.RedirectURI)
	switch {
	case errors.Is(err, ErrInvalidGrant):
		slog.WarnContext(r.Context(), "auth: device code refused", "err", err)
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidGrant, "invalid, used or expired code; sign in again")
		return
	case errors.Is(err, ErrTooManyDevices):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeDeviceLimit, "device limit reached; revoke one first")
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "auth: device token", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	}
	slog.InfoContext(r.Context(), "device signed in", "username", u.Username, "device", dev.ID, "platform", dev.Platform)
	h.Audit.Record(r, audit.Event{
		Actor: audit.Actor{ID: u.ID, Name: u.Username}, Action: audit.DeviceAuthorize, TargetID: dev.ID, TargetName: dev.Name,
		Details: map[string]any{"platform": dev.Platform, "clientVersion": dev.ClientVersion},
	})
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, deviceTokenResponse{Token: token, Device: dev, User: u})
}
