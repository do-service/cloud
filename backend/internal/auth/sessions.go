// Package auth authenticates people: sessions, local passwords, OIDC
// providers and, later, app passwords. What an authenticated user may do is
// decided elsewhere (access, fs).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNoSession = errors.New("auth: no such session")

// User is the authenticated principal a request carries.
type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	IsAdmin     bool   `json:"isAdmin"`
}

// renewEvery bounds how often an active session's expiry is pushed out:
// sliding it on every request would turn each read into a write.
const renewEvery = time.Hour

// maxUserAgent keeps a hostile client from storing kilobytes per session.
const maxUserAgent = 512

type Sessions struct {
	db  *pgxpool.Pool
	ttl time.Duration
}

// NewSessions keeps sessions alive for ttl after their last use.
func NewSessions(db *pgxpool.Pool, ttl time.Duration) *Sessions {
	return &Sessions{db: db, ttl: ttl}
}

// Create starts a session for userID and returns the raw token for the
// cookie, and when it expires.
func (s *Sessions) Create(ctx context.Context, userID string, ip netip.Addr, userAgent string) (string, time.Time, error) {
	var b [32]byte
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(b[:])
	raw := base64.RawURLEncoding.EncodeToString(b[:])
	if len(userAgent) > maxUserAgent {
		userAgent = userAgent[:maxUserAgent]
	}
	var ipArg *netip.Addr
	if ip.IsValid() {
		ipArg = &ip
	}
	var expires time.Time
	err := s.db.QueryRow(ctx, `
		INSERT INTO sessions (id_hash, user_id, expires_at, ip, user_agent)
		VALUES ($1, $2, now() + $3::interval, $4, $5)
		RETURNING expires_at`,
		hashToken(raw), userID, s.ttl, ipArg, userAgent).Scan(&expires)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create session: %w", err)
	}
	return raw, expires, nil
}

// Lookup returns the user behind a raw session token. A session past its
// expiry or of a disabled user is ErrNoSession — checked on every request,
// so disabling someone takes effect immediately. renewed is the new expiry
// when this call slid it forward (the cookie must follow), else zero.
func (s *Sessions) Lookup(ctx context.Context, raw string) (u User, renewed time.Time, err error) {
	var lastSeen time.Time
	err = s.db.QueryRow(ctx, `
		SELECT u.id, u.username, u.display_name, u.is_admin, s.last_seen_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id_hash = $1 AND s.expires_at > now() AND u.status = 'active'`,
		hashToken(raw)).Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin, &lastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, time.Time{}, ErrNoSession
	}
	if err != nil {
		return User{}, time.Time{}, fmt.Errorf("look up session: %w", err)
	}
	if time.Since(lastSeen) < renewEvery {
		return u, time.Time{}, nil
	}
	err = s.db.QueryRow(ctx, `
		UPDATE sessions SET last_seen_at = now(), expires_at = now() + $2::interval
		WHERE id_hash = $1 RETURNING expires_at`, hashToken(raw), s.ttl).Scan(&renewed)
	if errors.Is(err, pgx.ErrNoRows) {
		// Logged out by a concurrent request.
		return User{}, time.Time{}, ErrNoSession
	}
	if err != nil {
		return User{}, time.Time{}, fmt.Errorf("renew session: %w", err)
	}
	return u, renewed, nil
}

func (s *Sessions) Delete(ctx context.Context, raw string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE id_hash = $1`, hashToken(raw)); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteForUser logs userID out everywhere.
func (s *Sessions) DeleteForUser(ctx context.Context, userID string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete sessions: %w", err)
	}
	return nil
}

func hashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
