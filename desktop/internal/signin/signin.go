// Package signin connects the client to a cloud account: the sign-in in
// the system browser on the cloud's own domain (RFC 8252 loopback
// redirect with PKCE, docs/desktop-client.md «Вход»), and the device
// token it ends with, kept in the OS keyring. The app never sees a
// password and draws no sign-in form: the browser already knows the
// user's cookies, passwords and OIDC provider, and the app can't watch
// what is typed there.
package signin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/do-service/cloud/desktop/internal/api"
)

var (
	// ErrTimeout: the browser didn't come back in Options.Timeout.
	ErrTimeout = errors.New("signin: timed out waiting for the browser")
	// ErrAccessDenied: the user refused on the consent page.
	ErrAccessDenied = errors.New("signin: access denied in the browser")
	// ErrStateMismatch: the redirect isn't the answer to this sign-in
	// (an old tab, or someone else's request to the loopback port).
	ErrStateMismatch = errors.New("signin: the browser's answer doesn't belong to this sign-in")
)

// DefaultTimeout is how long Login waits for the browser: long enough to
// sign in with a password manager or a second factor.
const DefaultTimeout = 10 * time.Minute

type Options struct {
	// Server is the cloud's address; it is normalized (api.NormalizeServer).
	Server string
	// DeviceName is how the device is listed in the user's settings;
	// default DeviceName().
	DeviceName string
	// ClientVersion defaults to api.Version.
	ClientVersion string
	// OpenBrowser opens the consent page; default OpenBrowser. If it
	// fails, Login fails, unless OnURL is set: then the user can open
	// the URL it was given by hand.
	OpenBrowser func(url string) error
	// OnURL, when set, gets the consent page's URL before the browser is
	// opened, for the UI to show «если браузер не открылся…».
	OnURL func(url string)
	// Timeout defaults to DefaultTimeout.
	Timeout time.Duration
	// HTTPClient is for the requests to the server (/sync/info, the code
	// exchange); nil is api's default.
	HTTPClient *http.Client
}

// Result is a signed-in connection. Token is a secret: into the Store,
// and never into logs or output.
type Result struct {
	// Server is normalized.
	Server string
	api.DeviceToken
}

// Login signs in to opts.Server in the browser: it checks the server
// (/sync/info), listens on 127.0.0.1 at a random port, opens the consent
// page and waits for the browser to come back with a code, which it
// trades with its PKCE verifier for a device token. Cancelling ctx stops
// the wait (the «Отмена» button).
func Login(ctx context.Context, opts Options) (Result, error) {
	server, err := api.NormalizeServer(opts.Server)
	if err != nil {
		return Result{}, err
	}
	if opts.DeviceName == "" {
		opts.DeviceName = DeviceName()
	}
	if opts.ClientVersion == "" {
		opts.ClientVersion = api.Version
	}
	if opts.OpenBrowser == nil {
		opts.OpenBrowser = OpenBrowser
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	// Few retries: a mistyped address should fail while the user waits
	// at the form, not after a minute of backoff.
	client, err := api.New(server, api.Options{
		HTTPClient: opts.HTTPClient, ClientVersion: opts.ClientVersion,
		Retry: api.RetryPolicy{Attempts: 2, MinDelay: 500 * time.Millisecond, MaxDelay: time.Second},
	})
	if err != nil {
		return Result{}, err
	}
	// Before the browser: an address that isn't a cloud, or one this
	// client can't sync with, is better said now than after the user
	// has signed in.
	if _, err := client.Info(ctx); err != nil {
		return Result{}, err
	}

	// By IP, not "localhost": that may resolve elsewhere, and the server
	// accepts only 127.0.0.1 and [::1] redirects (RFC 8252 §8.3).
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Result{}, fmt.Errorf("signin: listen on loopback: %w", err)
	}
	redirectURI := "http://127.0.0.1:" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port) + "/cb"
	verifier := randomString(32)
	state := randomString(16)
	cb := &callback{
		state: state,
		exchange: func(ctx context.Context, code string) (api.DeviceToken, error) {
			return client.ExchangeCode(ctx, code, verifier, redirectURI)
		},
		done: make(chan outcome, 1),
	}
	srv := &http.Server{Handler: cb, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// Lets the page of the answer finish going out.
		_ = srv.Shutdown(ctx)
	}()

	page := authorizeURL(server, redirectURI, challenge(verifier), state, opts.DeviceName, opts.ClientVersion)
	if opts.OnURL != nil {
		opts.OnURL(page)
	}
	if err := opts.OpenBrowser(page); err != nil && opts.OnURL == nil {
		return Result{}, fmt.Errorf("signin: open the browser: %w", err)
	}

	timer := time.NewTimer(opts.Timeout)
	defer timer.Stop()
	select {
	case o := <-cb.done:
		if o.err != nil {
			return Result{}, o.err
		}
		return Result{Server: server, DeviceToken: o.token}, nil
	case <-timer.C:
		cb.close()
		return Result{}, ErrTimeout
	case <-ctx.Done():
		cb.close()
		return Result{}, ctx.Err()
	}
}

func authorizeURL(server, redirectURI, challenge, state, name, version string) string {
	q := url.Values{
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"name":                  {name},
		"platform":              {Platform()},
		"client_version":        {version},
	}
	return server + "/devices/authorize?" + q.Encode()
}

// randomString is n random bytes, base64url without padding: for the
// PKCE verifier (32 bytes → 43 characters, RFC 7636 §4.1) and the state.
func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// challenge is the S256 code_challenge of verifier.
func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
