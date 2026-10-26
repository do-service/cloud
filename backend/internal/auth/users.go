package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidCredentials = errors.New("auth: invalid username or password")
	ErrInvalidUsername    = errors.New("auth: invalid username")
	ErrUsernameTaken      = errors.New("auth: username already taken")
	ErrWeakPassword       = errors.New("auth: password must be 8 to 1024 bytes")
)

// validUsername keeps usernames safe to put in URLs (/dav/files/{username})
// and readable in logs. Case-insensitive uniqueness comes from citext.
var validUsername = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

const (
	// NIST SP 800-63B's minimum for user-chosen passwords.
	minPassword = 8
	// Long enough for any passphrase or generated password, short enough
	// that a request body can't be used to make hashing expensive.
	maxPassword = 1024
)

// Users manages local accounts: those signing in with a password.
type Users struct {
	db *pgxpool.Pool
	// DefaultQuota is new accounts' quota in bytes (R16), nil unlimited.
	DefaultQuota *int64
}

func NewUsers(db *pgxpool.Pool) *Users { return &Users{db: db} }

func (u *Users) CreateLocal(ctx context.Context, username, displayName, password string, isAdmin bool) (User, error) {
	if !validUsername.MatchString(username) {
		return User{}, ErrInvalidUsername
	}
	if len(password) < minPassword || len(password) > maxPassword {
		return User{}, ErrWeakPassword
	}
	if displayName == "" {
		displayName = username
	}
	hash, err := hashPassword(ctx, password)
	if err != nil {
		return User{}, err
	}
	var user User
	err = u.db.QueryRow(ctx, `
		INSERT INTO users (username, display_name, password_hash, is_admin, quota_bytes) VALUES ($1, $2, $3, $4, $5)
		RETURNING id, username, display_name, is_admin`,
		username, displayName, hash, isAdmin, u.DefaultQuota).Scan(&user.ID, &user.Username, &user.DisplayName, &user.IsAdmin)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return User{}, ErrUsernameTaken
	}
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

// Authenticate checks a username and password. Every failure — unknown
// user, wrong password, disabled account, account without a password
// (OIDC-only) — is the same ErrInvalidCredentials after the same amount of
// hashing, so neither the answer nor its timing tells which usernames
// exist.
func (u *Users) Authenticate(ctx context.Context, username, password string) (User, error) {
	if len(password) > maxPassword {
		return User{}, ErrInvalidCredentials
	}
	var user User
	var hash *string
	var status string
	err := u.db.QueryRow(ctx, `
		SELECT id, username, display_name, is_admin, password_hash, status FROM users WHERE username = $1`,
		username).Scan(&user.ID, &user.Username, &user.DisplayName, &user.IsAdmin, &hash, &status)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return User{}, fmt.Errorf("look up user: %w", err)
	}
	encoded := dummyHash()
	if err == nil && hash != nil {
		encoded = *hash
	}
	ok, verr := verifyPassword(ctx, encoded, password)
	if verr != nil {
		return User{}, fmt.Errorf("verify password of %s: %w", username, verr)
	}
	if err != nil || hash == nil || !ok || status != "active" {
		return User{}, ErrInvalidCredentials
	}
	return user, nil
}

// HasAdmin reports whether any active administrator exists.
func (u *Users) HasAdmin(ctx context.Context) (bool, error) {
	var exists bool
	err := u.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE is_admin AND status = 'active')`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check for admin: %w", err)
	}
	return exists, nil
}

// Delete removes an account; sessions and identities go with it.
func (u *Users) Delete(ctx context.Context, id string) error {
	if _, err := u.db.Exec(ctx, `DELETE FROM users WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return nil
}
