package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// The desktop sign-in (E12-2) is RFC 8252's loopback flow with PKCE: the
// client listens on 127.0.0.1, opens the consent page in the system
// browser, the page sends the browser back to the client with a one-time
// code, and the client trades the code and its code_verifier for a
// device token. The browser never sees the token; whoever intercepts the
// redirect has the code without the verifier.

var (
	ErrInvalidRedirect  = errors.New("auth: redirect_uri must be http://127.0.0.1:<port>/<path> or http://[::1]:<port>/<path>")
	ErrInvalidChallenge = errors.New("auth: code_challenge must be an S256 challenge")
	ErrInvalidState     = errors.New("auth: state must be 1 to 200 characters")
	// ErrInvalidGrant is every way an exchange can fail — unknown, used or
	// expired code, wrong verifier or redirect_uri, account blocked since —
	// told apart only in the server's log: the caller learns nothing it
	// could probe with.
	ErrInvalidGrant = errors.New("auth: invalid grant")
)

const (
	// deviceCodeTTL is long enough to switch from the browser back to the
	// app, far too short to be worth stealing a code from a history.
	deviceCodeTTL = 120 * time.Second
	maxState      = 200
	// challengeLen is base64url(SHA-256) without padding.
	challengeLen = 43
)

// DeviceGrant is what the client asks for on the consent page.
type DeviceGrant struct {
	RedirectURI   string
	CodeChallenge string
	State         string
	Name          string
	Platform      string
	ClientVersion string
}

// checkRedirectURI accepts only a loopback redirect by IP literal, as RFC
// 8252 §7.3 and §8.3 recommend: "localhost" may resolve elsewhere, and
// any other host would hand the code to someone else. The URI must be in
// canonical form, so what the client registered and what it sends to
// /token compare byte for byte, and the code can be appended as the only
// query.
func checkRedirectURI(raw string) error {
	var rest string
	switch {
	case strings.HasPrefix(raw, "http://127.0.0.1:"):
		rest = strings.TrimPrefix(raw, "http://127.0.0.1:")
	case strings.HasPrefix(raw, "http://[::1]:"):
		rest = strings.TrimPrefix(raw, "http://[::1]:")
	default:
		return ErrInvalidRedirect
	}
	port, path, ok := strings.Cut(rest, "/")
	if !ok || port == "" || port[0] == '0' {
		return ErrInvalidRedirect
	}
	n, err := strconv.Atoi(port)
	// Below 1024 needs root; a client never listens there.
	if err != nil || n < 1024 || n > 65535 || strconv.Itoa(n) != port {
		return ErrInvalidRedirect
	}
	if strings.ContainsAny(path, "?#\\") {
		return ErrInvalidRedirect
	}
	u, err := url.Parse(raw)
	if err != nil || u.String() != raw {
		return ErrInvalidRedirect
	}
	return nil
}

func checkChallenge(challenge, method string) error {
	if method != "S256" || len(challenge) != challengeLen {
		return ErrInvalidChallenge
	}
	// Strict: the unused low bits of the last character must be zero, so
	// a challenge has exactly one spelling.
	if b, err := base64.RawURLEncoding.Strict().DecodeString(challenge); err != nil || len(b) != sha256.Size {
		return ErrInvalidChallenge
	}
	return nil
}

func checkState(state string) error {
	if state == "" || utf8.RuneCountInString(state) > maxState || !utf8.ValidString(state) {
		return ErrInvalidState
	}
	return nil
}

// checkGrant validates g and returns the device name as it is stored.
func checkGrant(g DeviceGrant, method string) (string, error) {
	if err := checkRedirectURI(g.RedirectURI); err != nil {
		return "", err
	}
	if err := checkChallenge(g.CodeChallenge, method); err != nil {
		return "", err
	}
	if err := checkState(g.State); err != nil {
		return "", err
	}
	return checkDevice(g.Name, g.Platform, g.ClientVersion)
}

// withQuery appends q to a redirect_uri checkRedirectURI accepted, which
// has no query of its own.
func withQuery(redirectURI string, q url.Values) string {
	return redirectURI + "?" + q.Encode()
}

// DenyRedirect is where the consent page sends the browser when the user
// refuses: RFC 6749's access_denied, so the client stops waiting.
func DenyRedirect(redirectURI, state string) (string, error) {
	if err := checkRedirectURI(redirectURI); err != nil {
		return "", err
	}
	if err := checkState(state); err != nil {
		return "", err
	}
	return withQuery(redirectURI, url.Values{"error": {"access_denied"}, "state": {state}}), nil
}

// Authorize issues a one-time code for userID's consent to g and returns
// the redirect that carries it to the client. The device itself is made
// at the exchange, so a code that is never redeemed leaves nothing behind;
// the limit is checked here too, so the user hears of it on the page.
func (d *Devices) Authorize(ctx context.Context, userID string, g DeviceGrant, method string) (string, error) {
	name, err := checkGrant(g, method)
	if err != nil {
		return "", err
	}
	var count int
	if err := d.db.QueryRow(ctx, `SELECT count(*) FROM devices WHERE user_id = $1`, userID).Scan(&count); err != nil {
		return "", fmt.Errorf("count devices: %w", err)
	}
	if count >= MaxDevices {
		return "", ErrTooManyDevices
	}
	code := randomToken()
	_, err = d.db.Exec(ctx, `
		INSERT INTO device_codes (code_hash, user_id, name, platform, client_version, code_challenge, redirect_uri, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now() + $8 * interval '1 second')`,
		hashToken(code), userID, name, g.Platform, g.ClientVersion, g.CodeChallenge, g.RedirectURI, int(deviceCodeTTL.Seconds()))
	if err != nil {
		return "", fmt.Errorf("issue device code: %w", err)
	}
	return withQuery(g.RedirectURI, url.Values{"code": {code}, "state": {g.State}}), nil
}

// checkVerifier is RFC 7636 §4.1: 43 to 128 unreserved characters.
func checkVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, c := range []byte(v) {
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	return true
}

// Exchange redeems a code for a new device of the user who consented and
// returns it with its token. The code is deleted by the lookup itself,
// whatever comes after: a second try with another verifier finds nothing,
// so a stolen code can't be brute-forced. Expired codes of anyone are
// swept on the way: the table only ever holds two minutes' worth.
func (d *Devices) Exchange(ctx context.Context, code, verifier, redirectURI string) (User, Device, string, error) {
	if _, err := d.db.Exec(ctx, `DELETE FROM device_codes WHERE expires_at <= now()`); err != nil {
		return User{}, Device{}, "", fmt.Errorf("sweep device codes: %w", err)
	}
	if code == "" {
		return User{}, Device{}, "", ErrInvalidGrant
	}
	var (
		u                                       User
		active                                  bool
		name, platform, version, challenge, uri string
	)
	err := d.db.QueryRow(ctx, `
		DELETE FROM device_codes c USING users u
		WHERE c.code_hash = $1 AND u.id = c.user_id AND c.expires_at > now()
		RETURNING u.id, u.username, u.display_name, u.is_admin, u.status = 'active',
			c.name, c.platform, c.client_version, c.code_challenge, c.redirect_uri`,
		hashToken(code)).Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin, &active, &name, &platform, &version, &challenge, &uri)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, Device{}, "", fmt.Errorf("%w: unknown, used or expired code", ErrInvalidGrant)
	}
	if err != nil {
		return User{}, Device{}, "", fmt.Errorf("redeem device code: %w", err)
	}
	sum := sha256.Sum256([]byte(verifier))
	// Constant time, though both secrets are already spent with the
	// row: it costs nothing and leaves no timing to reason about.
	if !checkVerifier(verifier) || subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) != 1 {
		return User{}, Device{}, "", fmt.Errorf("%w: code_verifier does not match", ErrInvalidGrant)
	}
	if subtle.ConstantTimeCompare([]byte(redirectURI), []byte(uri)) != 1 {
		return User{}, Device{}, "", fmt.Errorf("%w: redirect_uri does not match", ErrInvalidGrant)
	}
	if !active {
		return User{}, Device{}, "", fmt.Errorf("%w: account disabled", ErrInvalidGrant)
	}
	dev, token, err := d.Create(ctx, u.ID, name, platform, version)
	if err != nil {
		return User{}, Device{}, "", err
	}
	return u, dev, token, nil
}
