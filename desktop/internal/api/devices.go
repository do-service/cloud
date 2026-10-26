package api

import (
	"context"
	"net/http"
)

type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
}

// DeviceToken is what a sign-in ends with. Token is a secret: it goes to
// the keyring and nowhere else (no logs, no output).
type DeviceToken struct {
	Token  string `json:"token"`
	Device Device `json:"device"`
	User   User   `json:"user"`
}

// ExchangeCode trades the one-time code from the sign-in redirect and the
// PKCE verifier for a device token (POST /api/v1/devices/token, no auth).
// redirectURI must be the one the sign-in started with. A refused code is
// ErrInvalidGrant: start the sign-in over.
//
// Not retried after a failure midway: the code is single-use, so a second
// try after the first went through is refused anyway.
func (c *Client) ExchangeCode(ctx context.Context, code, verifier, redirectURI string) (DeviceToken, error) {
	var t DeviceToken
	err := c.do(ctx, call{
		method: http.MethodPost, path: "/api/v1/devices/token", noAuth: true, out: &t,
		in: map[string]string{"code": code, "codeVerifier": verifier, "redirectUri": redirectURI},
	})
	return t, err
}

// SignOut revokes this client's device token on the server
// (DELETE /api/v1/me/devices/current). A token revoked already (from the
// web, say) is ErrUnauthorized: signed out either way.
func (c *Client) SignOut(ctx context.Context) error {
	return c.do(ctx, call{method: http.MethodDelete, path: "/api/v1/me/devices/current"})
}
