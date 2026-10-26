package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/httpx"
)

const (
	// The flow cookie carries state, nonce and the PKCE verifier from
	// /start to /callback. It needn't be signed: an attacker can't set it
	// in a victim's browser (__Host- over https), and tampering with one's
	// own only breaks one's own sign-in.
	hostFlowCookieName  = "__Host-oidc"
	plainFlowCookieName = "oidc"
	// flowTTL is how long the user may spend at the provider's login page.
	flowTTL = 10 * time.Minute

	// discoveryRetry spaces out discovery attempts while a provider is
	// down, so a click storm doesn't become a request storm against it.
	discoveryRetry = 10 * time.Second
	// idpTimeout bounds each call to a provider (discovery, token, keys).
	idpTimeout = 15 * time.Second
)

// OIDC holds the enabled OpenID Connect providers. Each is discovered on
// first use, not at startup, so a provider being down doesn't keep the
// app (or the other providers) from starting.
type OIDC struct {
	list   []*oidcProvider
	bySlug map[string]*oidcProvider
	client *http.Client
	// base is PUBLIC_BASE_URL: where providers send the browser back.
	base *url.URL
}

type oidcProvider struct {
	Provider
	redirectURL string

	mu       sync.Mutex
	ready    *oidcEndpoints
	failedAt time.Time
	lastErr  error
}

type oidcEndpoints struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewOIDC serves providers with redirect URIs under baseURL
// (PUBLIC_BASE_URL): {baseURL}/api/v1/auth/oidc/{slug}/callback is what
// has to be registered at each provider.
func NewOIDC(baseURL string, providers []Provider) *OIDC {
	o := &OIDC{bySlug: map[string]*oidcProvider{}, client: &http.Client{Timeout: idpTimeout}}
	// config.Parse has checked it is an absolute URL.
	o.base, _ = url.Parse(strings.TrimRight(baseURL, "/"))
	for _, p := range providers {
		op := &oidcProvider{Provider: p, redirectURL: strings.TrimRight(baseURL, "/") + "/api/v1/auth/oidc/" + p.Slug + "/callback"}
		o.list = append(o.list, op)
		o.bySlug[p.Slug] = op
	}
	return o
}

// clientContext routes go-oidc's and oauth2's requests through o.client.
func (o *OIDC) clientContext(ctx context.Context) context.Context {
	return context.WithValue(oidc.ClientContext(ctx, o.client), oauth2.HTTPClient, o.client)
}

func (o *OIDC) endpoints(ctx context.Context, p *oidcProvider) (*oidcEndpoints, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ready != nil {
		return p.ready, nil
	}
	if time.Since(p.failedAt) < discoveryRetry {
		return nil, p.lastErr
	}
	// Detached from the request: a client hanging up mid-discovery
	// mustn't count as the provider being down for everyone else.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), idpTimeout)
	defer cancel()
	disc, err := oidc.NewProvider(o.clientContext(ctx), p.Issuer)
	if err != nil {
		p.failedAt, p.lastErr = time.Now(), fmt.Errorf("discover %s: %w", p.Issuer, err)
		return nil, p.lastErr
	}
	p.ready = &oidcEndpoints{
		oauth: oauth2.Config{
			ClientID:     p.ClientID,
			ClientSecret: p.ClientSecret,
			Endpoint:     disc.Endpoint(),
			RedirectURL:  p.redirectURL,
			Scopes:       p.Scopes,
		},
		verifier: disc.Verifier(&oidc.Config{ClientID: p.ClientID}),
	}
	return p.ready, nil
}

// flow is the flow cookie's content.
type flow struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"r"`
}

func flowCookieName(c Cookies) string {
	if c.Secure {
		return hostFlowCookieName
	}
	return plainFlowCookieName
}

func randomToken() string {
	var b [32]byte
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// safeNext keeps the post-login redirect on this site: only an absolute
// path is accepted, and "//host" or "/\host" (which browsers treat as
// another origin) are not, so the login link can't become an open
// redirect.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || strings.ContainsAny(next, "\r\n\t") {
		return "/"
	}
	return next
}

type providerInfo struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type providersResponse struct {
	Password  bool           `json:"password"`
	Providers []providerInfo `json:"providers"`
}

// providers lists the sign-in options for the login page.
func (h *Handlers) providers(w http.ResponseWriter, r *http.Request) {
	resp := providersResponse{Password: h.Users != nil, Providers: []providerInfo{}}
	if h.OIDC != nil {
		for _, p := range h.OIDC.list {
			resp.Providers = append(resp.Providers, providerInfo{Slug: p.Slug, Name: p.Name})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// oidcStart sends the browser to the provider's login page.
func (h *Handlers) oidcStart(w http.ResponseWriter, r *http.Request) {
	p := h.OIDC.bySlug[r.PathValue("slug")]
	if p == nil {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such sign-in provider")
		return
	}
	// The flow cookie must be set on the host the provider sends the
	// browser back to, PUBLIC_BASE_URL's: a sign-in begun on another name
	// for the server (127.0.0.1 for localhost, say) would come back
	// without it and fail. So begin it again there — once: a proxy that
	// passes another Host would otherwise send the browser round forever.
	if b := h.OIDC.base; b != nil && b.Host != "" && !strings.EqualFold(r.Host, b.Host) && r.URL.Query().Get("canonical") == "" {
		q := r.URL.Query()
		q.Set("canonical", "1")
		http.Redirect(w, r, b.Scheme+"://"+b.Host+r.URL.Path+"?"+q.Encode(), http.StatusFound)
		return
	}
	ep, err := h.OIDC.endpoints(r.Context(), p)
	if err != nil {
		slog.WarnContext(r.Context(), "auth: oidc provider unavailable", "provider", p.Slug, "err", err)
		loginFailed(w, r, httpx.CodeProviderUnavailable)
		return
	}
	f := flow{Provider: p.Slug, State: randomToken(), Nonce: randomToken(), Verifier: oauth2.GenerateVerifier(), Next: safeNext(r.URL.Query().Get("next"))}
	raw, err := json.Marshal(f)
	if err != nil {
		slog.ErrorContext(r.Context(), "auth: oidc start", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flowCookieName(h.Cookies),
		Value:    base64.RawURLEncoding.EncodeToString(raw),
		Path:     "/",
		MaxAge:   int(flowTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.Cookies.Secure,
		// Lax lets the cookie ride along on the provider's top-level
		// redirect back to /callback, which is a cross-site navigation.
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, ep.oauth.AuthCodeURL(f.State, oidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier)), http.StatusFound)
}

// oidcCallback completes the flow: checks state, redeems the code with
// the PKCE verifier, verifies the ID token and its nonce, then signs the
// user in as a password login would.
func (h *Handlers) oidcCallback(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	p := h.OIDC.bySlug[slug]
	if p == nil {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such sign-in provider")
		return
	}
	// The flow is single-use whatever happens next.
	name := flowCookieName(h.Cookies)
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: h.Cookies.Secure, SameSite: http.SameSiteLaxMode})

	fail := func(code, msg string, args ...any) {
		slog.WarnContext(r.Context(), "auth: oidc sign-in failed: "+msg, append([]any{"provider", slug}, args...)...)
		h.Audit.Record(r, audit.Event{Action: audit.LoginFailed, Details: map[string]any{"method": "oidc", "provider": slug, "reason": msg}})
		loginFailed(w, r, code)
	}
	var f flow
	ck, err := r.Cookie(name)
	if err != nil {
		fail(httpx.CodeOIDCFailed, "no flow cookie (expired, or started in another browser)")
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(ck.Value)
	if err != nil || json.Unmarshal(raw, &f) != nil {
		fail(httpx.CodeOIDCFailed, "malformed flow cookie")
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		// Typically the user cancelled at the provider.
		fail(httpx.CodeOIDCFailed, "provider returned an error", "error", e, "description", q.Get("error_description"))
		return
	}
	if f.Provider != slug || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.State)) != 1 {
		fail(httpx.CodeOIDCFailed, "state mismatch")
		return
	}
	ep, err := h.OIDC.endpoints(r.Context(), p)
	if err != nil {
		fail(httpx.CodeProviderUnavailable, "provider unavailable", "err", err)
		return
	}
	ctx := h.OIDC.clientContext(r.Context())
	tok, err := ep.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		fail(httpx.CodeOIDCFailed, "code exchange", "err", err)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		fail(httpx.CodeOIDCFailed, "token response without id_token")
		return
	}
	idt, err := ep.verifier.Verify(ctx, rawID)
	if err != nil {
		fail(httpx.CodeOIDCFailed, "id token rejected", "err", err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(f.Nonce)) != 1 {
		fail(httpx.CodeOIDCFailed, "nonce mismatch")
		return
	}
	var claims struct {
		Email             string   `json:"email"`
		EmailVerified     flexBool `json:"email_verified"`
		PreferredUsername string   `json:"preferred_username"`
		Name              string   `json:"name"`
	}
	if err := idt.Claims(&claims); err != nil {
		fail(httpx.CodeOIDCFailed, "id token claims", "err", err)
		return
	}

	u, created, err := h.Users.SignInOIDC(r.Context(), p.Provider, Claims{
		Subject:           idt.Subject,
		Email:             claims.Email,
		EmailVerified:     bool(claims.EmailVerified),
		PreferredUsername: claims.PreferredUsername,
		Name:              claims.Name,
	})
	switch {
	case errors.Is(err, ErrSignupDisabled):
		fail(httpx.CodeSignupDisabled, "no account and the provider doesn't create them", "subject", idt.Subject)
		return
	case errors.Is(err, ErrAccountDisabled):
		fail(httpx.CodeAccountDisabled, "account disabled", "subject", idt.Subject)
		return
	case errors.Is(err, ErrEmailAmbiguous):
		fail(httpx.CodeOIDCFailed, "several accounts share the verified email; link manually", "subject", idt.Subject)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "auth: oidc sign-in", "provider", slug, "err", err)
		loginFailed(w, r, httpx.CodeInternal)
		return
	}
	if created {
		if err := h.setUpNewUser(r.Context(), u); err != nil {
			slog.ErrorContext(r.Context(), "auth: set up new oidc user", "provider", slug, "username", u.Username, "err", err)
			loginFailed(w, r, httpx.CodeInternal)
			return
		}
		slog.InfoContext(r.Context(), "created user on first OIDC sign-in", "provider", slug, "username", u.Username)
	}
	if err := h.startSession(w, r, u); err != nil {
		slog.ErrorContext(r.Context(), "auth: oidc sign-in", "provider", slug, "err", err)
		loginFailed(w, r, httpx.CodeInternal)
		return
	}
	h.Audit.Record(r, audit.Event{Actor: audit.Actor{ID: u.ID, Name: u.Username}, Action: audit.Login,
		Details: map[string]any{"method": "oidc", "provider": slug, "created": created}})
	http.Redirect(w, r, f.Next, http.StatusFound)
}

// setUpNewUser runs OnUserCreated; if that fails the account is removed
// again, so the next attempt starts over instead of finding an account
// without a home folder.
func (h *Handlers) setUpNewUser(ctx context.Context, u User) error {
	if h.OnUserCreated == nil {
		return nil
	}
	err := h.OnUserCreated(ctx, u)
	if err == nil {
		return nil
	}
	if derr := h.Users.Delete(context.WithoutCancel(ctx), u.ID); derr != nil {
		err = errors.Join(err, derr)
	}
	return err
}

// loginFailed sends a browser that is mid-redirect back to the login page
// with an error code to show: a JSON error body would leave the user on a
// blank page.
func loginFailed(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/login?error="+url.QueryEscape(code), http.StatusFound)
}

// flexBool accepts email_verified as a JSON boolean or as the string
// "true"/"false", which some providers send.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	switch strings.ToLower(string(data)) {
	case "true", `"true"`:
		*b = true
	default:
		*b = false
	}
	return nil
}
