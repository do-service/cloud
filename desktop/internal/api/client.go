// Package api is the client of the cloud server's API for the sync
// engine (docs/desktop-client.md): /sync/*, the nodes REST API with
// If-Match, downloads with Range/If-Range, resumable uploads over tus,
// and the device sign-in exchange. One Client serves one connection
// (server and account) and authenticates with its device token as
// Bearer.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/time/rate"
)

// Version is the client's version, set at build time
// (-ldflags "-X …/internal/api.Version=1.2.3"). The server compares it
// with its minClientVersion and records it with the device.
var Version = "0.0.0-dev"

// TokenSource hands out the connection's device token for each request:
// it lives in the OS keyring (internal/signin), not in the client.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a TokenSource of one token.
type StaticToken string

func (t StaticToken) Token(context.Context) (string, error) { return string(t), nil }

// RetryPolicy is how a failed request is repeated: up to Attempts tries
// in all, waiting a random delay between half and all of
// MinDelay·2ⁿ (at most MaxDelay), or the server's Retry-After.
type RetryPolicy struct {
	// Attempts counts the first try; 1 disables retries.
	Attempts int
	MinDelay time.Duration
	MaxDelay time.Duration
}

// DefaultRetry rides out a server restart or a short network outage
// (~1 min in all) without holding a transfer slot for long.
var DefaultRetry = RetryPolicy{Attempts: 6, MinDelay: 500 * time.Millisecond, MaxDelay: 30 * time.Second}

// maxRetryAfter caps a Retry-After: a misconfigured proxy asking for an
// hour mustn't stall a transfer that long.
const maxRetryAfter = 5 * time.Minute

// DefaultChunkSize is a tus PATCH's size: big enough that per-request
// overhead doesn't matter on a fast link, small enough that an
// interruption costs little (tusd keeps what arrived anyway).
const DefaultChunkSize = 64 << 20

type Options struct {
	// Token authenticates requests; nil sends none (Info, ExchangeCode
	// need none).
	Token TokenSource
	// HTTPClient defaults to one with connection and handshake timeouts
	// but no overall timeout: downloads and long polls run long.
	HTTPClient *http.Client
	// ClientVersion defaults to Version.
	ClientVersion string
	// Retry defaults to DefaultRetry.
	Retry RetryPolicy
	// RequestTimeout bounds one attempt of a JSON request (default 60 s);
	// a long poll gets its wait on top.
	RequestTimeout time.Duration
	// ChunkSize of tus uploads, default DefaultChunkSize.
	ChunkSize int64
	// UploadLimit and DownloadLimit, when set, cap the bytes per second
	// of all uploads and downloads of this client (and any other client
	// sharing the limiter): the user's bandwidth setting.
	UploadLimit, DownloadLimit *rate.Limiter
}

// Client talks to one server. It is safe for concurrent use.
type Client struct {
	base    *url.URL
	server  string
	hc      *http.Client
	token   TokenSource
	ua      string
	version string
	retry   RetryPolicy
	timeout time.Duration
	chunk   int64
	upLim   *rate.Limiter
	downLim *rate.Limiter
	// sleep waits between retries; tests make it instant.
	sleep func(ctx context.Context, d time.Duration) error
}

// New makes a client of server, which is normalized (NormalizeServer).
func New(server string, opts Options) (*Client, error) {
	norm, err := NormalizeServer(server)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(norm)
	if err != nil {
		return nil, err
	}
	c := &Client{
		base: base, server: norm, hc: opts.HTTPClient, token: opts.Token, version: opts.ClientVersion,
		retry: opts.Retry, timeout: opts.RequestTimeout, chunk: opts.ChunkSize,
		upLim: opts.UploadLimit, downLim: opts.DownloadLimit, sleep: sleepCtx,
	}
	if c.hc == nil {
		c.hc = &http.Client{Transport: defaultTransport()}
	}
	if c.version == "" {
		c.version = Version
	}
	if c.retry.Attempts <= 0 {
		c.retry = DefaultRetry
	}
	if c.timeout <= 0 {
		c.timeout = 60 * time.Second
	}
	if c.chunk <= 0 {
		c.chunk = DefaultChunkSize
	}
	c.ua = "cloud-desktop/" + c.version + " (" + runtime.GOOS + ")"
	return c, nil
}

// Server is the normalized server address: the connection's identity
// together with the user id.
func (c *Client) Server() string { return c.server }

func defaultTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.MaxIdleConnsPerHost = 16
	return t
}

// NormalizeServer gives a server address one spelling, so one server
// isn't two connections and its keyring entry is found again: scheme and
// host lower-cased, the default port dropped, no trailing '/', no
// credentials, query or fragment. Plain http is only allowed to this
// machine (development): a token would otherwise travel in the clear.
//
// state.NormalizeServer implements the same rules for the connections
// registry; the two must agree.
func NormalizeServer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("api: server address: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "https" && scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("api: server address %q: want http(s)://host", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("api: server address %q: no user, query or fragment", raw)
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if scheme == "http" && !IsLoopback(host) {
		return "", fmt.Errorf("api: server address %q: http only for localhost, use https", raw)
	}
	hostport := host
	if strings.Contains(host, ":") {
		hostport = "[" + host + "]"
	}
	if port != "" {
		hostport = net.JoinHostPort(host, port)
	}
	return scheme + "://" + hostport + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// IsLoopback reports whether host is this machine: the only place plain
// http is allowed.
func IsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// endpoint is the URL of an API path ("/api/v1/…") with query q.
func (c *Client) endpoint(path string, q url.Values) string {
	s := c.server + path
	if len(q) > 0 {
		s += "?" + q.Encode()
	}
	return s
}

// resolve makes a reference from the server (tus' Location, a path)
// absolute against the server's address.
func (c *Client) resolve(ref string) (string, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("api: bad URL from the server %q: %w", ref, err)
	}
	abs := c.base.ResolveReference(u)
	// An upload URL is where a token goes: never to another host.
	if abs.Scheme != c.base.Scheme || abs.Host != c.base.Host {
		return "", fmt.Errorf("api: the server pointed to another host: %s", abs.Redacted())
	}
	return abs.String(), nil
}

// newRequest builds a request with the client's User-Agent and, if auth,
// the Bearer token.
func (c *Client) newRequest(ctx context.Context, method, url string, body io.Reader, auth bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	if auth && c.token != nil {
		tok, err := c.token.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("api: device token: %w", err)
		}
		if tok == "" {
			return nil, ErrUnauthorized
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return req, nil
}

// call is one JSON request.
type call struct {
	method string
	path   string
	query  url.Values
	// in is marshalled as the body, unless nil.
	in any
	// out receives a 2xx body, unless nil.
	out    any
	header http.Header
	noAuth bool
	// extra is added to the per-attempt timeout (a long poll's wait).
	extra time.Duration
}

func (c *Client) do(ctx context.Context, cl call) error {
	var body []byte
	if cl.in != nil {
		var err error
		if body, err = json.Marshal(cl.in); err != nil {
			return err
		}
	}
	return c.withRetry(ctx, idempotent(cl.method), func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, c.timeout+cl.extra)
		defer cancel()
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := c.newRequest(ctx, cl.method, c.endpoint(cl.path, cl.query), rd, !cl.noAuth)
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		for k, v := range cl.header {
			req.Header[k] = v
		}
		res, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		if res.StatusCode < 200 || res.StatusCode > 299 {
			return readError(res)
		}
		defer res.Body.Close()
		if cl.out == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxErrorBody))
			return nil
		}
		if err := json.NewDecoder(res.Body).Decode(cl.out); err != nil {
			return fmt.Errorf("api: %s %s: decode: %w", cl.method, cl.path, err)
		}
		return nil
	})
}

func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	}
	return false
}

// withRetry runs fn until it succeeds, fails in a way retrying can't
// fix, or the policy's attempts run out. An idempotent request is
// repeated after a server error (5xx), 429 and any network failure; a
// non-idempotent one (POST, PATCH) only when the server certainly didn't
// act on it: 429, or no connection made at all.
func (c *Client) withRetry(ctx context.Context, idem bool, fn func(ctx context.Context) error) error {
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil || ctx.Err() != nil || attempt >= c.retry.Attempts || !retryable(err, idem) {
			return err
		}
		if err := c.sleep(ctx, c.delay(attempt, err)); err != nil {
			return err
		}
	}
}

// permanent wraps an error withRetry must not retry, whatever it wraps.
type permanent struct{ err error }

func (e permanent) Error() string { return e.err.Error() }
func (e permanent) Unwrap() error { return e.err }

func retryable(err error, idem bool) bool {
	if errors.As(err, new(permanent)) {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		if ae.Status == http.StatusTooManyRequests {
			return true
		}
		return idem && ae.Status >= 500 && ae.Status != http.StatusNotImplemented && ae.Status != http.StatusInsufficientStorage
	}
	if errors.Is(err, ErrUnauthorized) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && !idem {
		return false
	}
	if idem {
		// Anything else is the network or a cut-off body: try again.
		return isNetwork(err)
	}
	return notSent(err)
}

// isNetwork tells transport failures from the client's own errors (a
// bad argument, a local file that can't be read).
func isNetwork(err error) bool {
	var ne net.Error
	var ue *url.Error
	return errors.As(err, &ne) || errors.As(err, &ue) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, ErrTruncated) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, context.DeadlineExceeded)
}

// notSent reports a failure to connect at all: the request never
// reached the server, so even a POST is safe to repeat.
func notSent(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func (c *Client) delay(attempt int, err error) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		return min(ae.RetryAfter, maxRetryAfter)
	}
	d := c.retry.MinDelay << (attempt - 1)
	if d <= 0 || d > c.retry.MaxDelay {
		d = c.retry.MaxDelay
	}
	// Jitter, so the clients of a server that just came back don't all
	// return in the same second.
	return d/2 + rand.N(d/2+1)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// quoteETag makes an ETag from JSON (unquoted, as the API gives them)
// an HTTP entity tag.
func quoteETag(etag string) string {
	if strings.HasPrefix(etag, `"`) || strings.HasPrefix(etag, `W/"`) {
		return etag
	}
	return `"` + etag + `"`
}

// unquoteETag is quoteETag's inverse, for ETag headers.
func unquoteETag(etag string) string {
	return strings.Trim(strings.TrimPrefix(etag, "W/"), `"`)
}
