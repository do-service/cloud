package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNoAppPassword      = errors.New("auth: no such app password")
	ErrInvalidName        = errors.New("auth: name must be 1 to 100 characters")
	ErrTooManyAppPassword = errors.New("auth: too many app passwords")
)

const (
	// appPasswordLen characters of a 32-letter alphabet: 125 random bits,
	// beyond guessing even at the speed a fast hash allows.
	appPasswordLen      = 25
	appPasswordAlphabet = "abcdefghijklmnopqrstuvwxyz234567"
	// appPasswordGroup splits the password for reading it out
	// ("abcde-fghij-…"); the dashes are optional when typing it back.
	appPasswordGroup = 5
	// MaxAppPasswords per user: one per device is the use, and the cap
	// keeps a script from filling the table.
	MaxAppPasswords = 50
	maxAppName      = 100
	// appUsedEvery bounds how often last_used_at is written: clients
	// authenticate every request, and "used today" is all the profile
	// needs to show.
	appUsedEvery = time.Hour
)

// AppPassword is a password's metadata; the password itself is shown
// only once, at creation.
type AppPassword struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

// AppPasswords manages passwords for WebDAV and other non-browser clients.
type AppPasswords struct {
	db *pgxpool.Pool
}

func NewAppPasswords(db *pgxpool.Pool) *AppPasswords { return &AppPasswords{db: db} }

func generateAppPassword() string {
	var b [appPasswordLen]byte
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(b[:])
	var out strings.Builder
	for i, c := range b {
		if i > 0 && i%appPasswordGroup == 0 {
			out.WriteByte('-')
		}
		// 256 is a multiple of 32, so the low five bits are uniform.
		out.WriteByte(appPasswordAlphabet[c&31])
	}
	return out.String()
}

// canonicalAppPassword strips the dashes and case a user may or may not
// type; ok is false for anything that can't be an app password.
func canonicalAppPassword(s string) (string, bool) {
	s = strings.ToLower(strings.ReplaceAll(s, "-", ""))
	if len(s) != appPasswordLen {
		return "", false
	}
	for _, r := range s {
		if !strings.ContainsRune(appPasswordAlphabet, r) {
			return "", false
		}
	}
	return s, true
}

// Create makes a password named name for userID and returns it, the one
// time it is available in the clear.
func (a *AppPasswords) Create(ctx context.Context, userID, name string) (AppPassword, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxAppName {
		return AppPassword{}, "", ErrInvalidName
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return AppPassword{}, "", fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The user row serializes concurrent creates, so the cap holds. The
	// count is a separate statement on purpose: under READ COMMITTED it
	// then sees the rows committed while this one waited for the lock,
	// which a subquery in the locking statement would not.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID); err != nil {
		return AppPassword{}, "", fmt.Errorf("lock user: %w", err)
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM app_passwords WHERE user_id = $1`, userID).Scan(&count); err != nil {
		return AppPassword{}, "", fmt.Errorf("count app passwords: %w", err)
	}
	if count >= MaxAppPasswords {
		return AppPassword{}, "", ErrTooManyAppPassword
	}
	password := generateAppPassword()
	canonical, _ := canonicalAppPassword(password)
	var p AppPassword
	err = tx.QueryRow(ctx, `
		INSERT INTO app_passwords (user_id, name, token_hash) VALUES ($1, $2, $3)
		RETURNING id, name, created_at, last_used_at`,
		userID, name, hashToken(canonical)).Scan(&p.ID, &p.Name, &p.CreatedAt, &p.LastUsedAt)
	if err != nil {
		return AppPassword{}, "", fmt.Errorf("create app password: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AppPassword{}, "", fmt.Errorf("commit: %w", err)
	}
	return p, password, nil
}

// List returns userID's passwords, newest first.
func (a *AppPasswords) List(ctx context.Context, userID string) ([]AppPassword, error) {
	rows, err := a.db.Query(ctx, `
		SELECT id, name, created_at, last_used_at FROM app_passwords
		WHERE user_id = $1 ORDER BY created_at DESC, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list app passwords: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AppPassword, error) {
		var p AppPassword
		err := row.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.LastUsedAt)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("list app passwords: %w", err)
	}
	return list, nil
}

// Delete revokes one of userID's passwords; someone else's is
// ErrNoAppPassword, as if it didn't exist.
func (a *AppPasswords) Delete(ctx context.Context, userID, id string) error {
	// Compared as text so a malformed id is simply not found; user_id
	// narrows the scan to a handful of rows anyway.
	tag, err := a.db.Exec(ctx, `DELETE FROM app_passwords WHERE id::text = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete app password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoAppPassword
	}
	return nil
}

// Authenticate checks a username and app password, as a WebDAV client
// sends them. A wrong password, another user's password, and a disabled
// account are all ErrInvalidCredentials.
func (a *AppPasswords) Authenticate(ctx context.Context, username, password string) (User, error) {
	canonical, ok := canonicalAppPassword(password)
	if !ok {
		return User{}, ErrInvalidCredentials
	}
	var u User
	var id string
	var lastUsed *time.Time
	err := a.db.QueryRow(ctx, `
		SELECT u.id, u.username, u.display_name, u.is_admin, a.id, a.last_used_at
		FROM app_passwords a JOIN users u ON u.id = a.user_id
		WHERE a.token_hash = $1 AND u.username = $2::citext AND u.status = 'active'`,
		hashToken(canonical), username).Scan(&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin, &id, &lastUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, fmt.Errorf("look up app password: %w", err)
	}
	if lastUsed == nil || time.Since(*lastUsed) >= appUsedEvery {
		if _, err := a.db.Exec(ctx, `UPDATE app_passwords SET last_used_at = now() WHERE id = $1`, id); err != nil {
			return User{}, fmt.Errorf("touch app password: %w", err)
		}
	}
	return u, nil
}
