package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// secretVersion leads every sealed value, so a later key rotation can tell
// which key a value was sealed under.
const secretVersion = 1

var (
	ErrNoSecretKey  = errors.New("auth: SECRET_KEY is not set")
	errSecretSealed = errors.New("auth: secret can't be opened (wrong SECRET_KEY or corrupted value)")
)

// SecretBox encrypts secrets stored in the database (AES-256-GCM).
type SecretBox struct {
	aead cipher.AEAD
}

// NewSecretBox takes the 32-byte SECRET_KEY. A nil key gives a box that
// refuses to seal or open, so a deployment without secrets needs no key.
func NewSecretBox(key []byte) (*SecretBox, error) {
	if key == nil {
		return &SecretBox{}, nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret key: %w", err)
	}
	return &SecretBox{aead: aead}, nil
}

// Seal encrypts plain, bound to context (e.g. the row's slug): a value
// copied into another row won't open there.
func (b *SecretBox) Seal(plain, context string) ([]byte, error) {
	if b.aead == nil {
		return nil, ErrNoSecretKey
	}
	out := make([]byte, 1+b.aead.NonceSize(), 1+b.aead.NonceSize()+len(plain)+b.aead.Overhead())
	out[0] = secretVersion
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(out[1:])
	return b.aead.Seal(out, out[1:], []byte(plain), []byte(context)), nil
}

func (b *SecretBox) Open(sealed []byte, context string) (string, error) {
	if b.aead == nil {
		return "", ErrNoSecretKey
	}
	n := b.aead.NonceSize()
	if len(sealed) < 1+n || sealed[0] != secretVersion {
		return "", errSecretSealed
	}
	plain, err := b.aead.Open(nil, sealed[1:1+n], sealed[1+n:], []byte(context))
	if err != nil {
		return "", errSecretSealed
	}
	return string(plain), nil
}
