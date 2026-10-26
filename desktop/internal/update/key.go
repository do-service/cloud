// Package update checks for new releases of the desktop client and
// installs them with Wails' pkg/updater (docs/desktop-client.md,
// «Обновления»): the user's mode (off, notify, download and ask), a check
// on start and every CheckInterval, the window's /app/v1/updates and the
// tray's item. Releases are signed with ed25519ph by cmd/signupdate in CI;
// the public key is compiled in, so verification fails closed.
//
// It doesn't import Wails' application package, so it builds and tests
// with CGO_ENABLED=0 like internal/app.
package update

import (
	"crypto/ed25519"
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"errors"
	"fmt"
)

// releaseKey is the release signing key's public half (PEM, PKIX). The
// private half is the UPDATE_SIGNING_KEY secret of the GitHub repository,
// used only by the release job.
//
//go:embed update-public-key.pem
var releaseKey []byte

// ReleaseKey is the public key releases are verified with, as
// updater.Config.PublicKey takes it.
func ReleaseKey() []byte { return releaseKey }

// ParsePublicKey reads an ed25519 public key in PEM (PKIX), the form of
// update-public-key.pem.
func ParsePublicKey(pemBytes []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("update: no PEM block in the public key")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("update: public key: %w", err)
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("update: public key is %T, not ed25519", k)
	}
	return pub, nil
}
