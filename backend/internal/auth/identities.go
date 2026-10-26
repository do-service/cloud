package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrSignupDisabled  = errors.New("auth: provider doesn't create accounts")
	ErrAccountDisabled = errors.New("auth: account is disabled")
	// ErrEmailAmbiguous: linking by email found several accounts with
	// the address, and picking one would be a guess.
	ErrEmailAmbiguous = errors.New("auth: several accounts have this email")
)

// errIdentityRace: a concurrent first sign-in of the same person created
// the identity first.
var errIdentityRace = errors.New("auth: identity created concurrently")

// Claims is what a provider vouched for about the person signing in.
type Claims struct {
	Subject           string
	Email             string
	EmailVerified     bool
	PreferredUsername string
	Name              string
}

// SignInOIDC finds or creates the account behind a provider's subject:
//  1. an existing identity signs in its user;
//  2. else, with LinkByEmail and a verified email, the identity attaches
//     to the one account with that email;
//  3. else, with AutoCreate, a new account without a password is made;
//  4. else ErrSignupDisabled.
//
// created reports case 3, for the caller to set up the new account.
func (u *Users) SignInOIDC(ctx context.Context, p Provider, c Claims) (user User, created bool, err error) {
	user, created, err = u.signInOIDC(ctx, p, c)
	if errors.Is(err, errIdentityRace) {
		// The other request's identity is committed now; this is case 1.
		user, created, err = u.signInOIDC(ctx, p, c)
	}
	return user, created, err
}

func (u *Users) signInOIDC(ctx context.Context, p Provider, c Claims) (User, bool, error) {
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return User{}, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	email := nullable(c.Email)
	var user User
	var status string
	err = tx.QueryRow(ctx, `
		UPDATE identities i SET email = $3, last_login_at = now() FROM users u
		WHERE i.provider_id = $1 AND i.subject = $2 AND u.id = i.user_id
		RETURNING u.id, u.username, u.display_name, u.is_admin, u.status`,
		p.ID, c.Subject, email).Scan(&user.ID, &user.Username, &user.DisplayName, &user.IsAdmin, &status)
	switch {
	case err == nil:
		if status != "active" {
			return User{}, false, ErrAccountDisabled
		}
		if err := tx.Commit(ctx); err != nil {
			return User{}, false, fmt.Errorf("commit: %w", err)
		}
		return user, false, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return User{}, false, fmt.Errorf("look up identity: %w", err)
	}

	if p.LinkByEmail && c.EmailVerified && c.Email != "" {
		rows, err := tx.Query(ctx, `SELECT id, username, display_name, is_admin, status FROM users WHERE email = $1 LIMIT 2`, c.Email)
		if err != nil {
			return User{}, false, fmt.Errorf("look up email: %w", err)
		}
		type match struct {
			User
			status string
		}
		matches, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (match, error) {
			var m match
			err := row.Scan(&m.ID, &m.Username, &m.DisplayName, &m.IsAdmin, &m.status)
			return m, err
		})
		if err != nil {
			return User{}, false, fmt.Errorf("look up email: %w", err)
		}
		if len(matches) > 1 {
			return User{}, false, ErrEmailAmbiguous
		}
		if len(matches) == 1 {
			user, status = matches[0].User, matches[0].status
		}
	}

	created := false
	if user.ID == "" {
		if !p.AutoCreate {
			return User{}, false, ErrSignupDisabled
		}
		if user, err = createOIDCUser(ctx, tx, c, u.DefaultQuota); err != nil {
			return User{}, false, err
		}
		created, status = true, "active"
	}
	if status != "active" {
		return User{}, false, ErrAccountDisabled
	}

	_, err = tx.Exec(ctx, `INSERT INTO identities (user_id, provider_id, subject, email) VALUES ($1, $2, $3, $4)`,
		user.ID, p.ID, c.Subject, email)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return User{}, false, errIdentityRace
	}
	if err != nil {
		return User{}, false, fmt.Errorf("insert identity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, false, fmt.Errorf("commit: %w", err)
	}
	return user, created, nil
}

// usernameTries is how many numbered variants (alice-2, alice-3, …) are
// tried before falling back to a random suffix.
const usernameTries = 20

func createOIDCUser(ctx context.Context, tx pgx.Tx, c Claims, quota *int64) (User, error) {
	base := usernameBase(c)
	display := c.Name
	if display == "" {
		display = base
	}
	// Only a verified email is kept on the account: users.email is what
	// LinkByEmail matches against, and an unverified one would let its
	// owner collect the real address holder's later sign-ins.
	var email *string
	if c.EmailVerified {
		email = nullable(c.Email)
	}
	for i := 1; i <= usernameTries+1; i++ {
		name := base
		switch {
		case i == usernameTries+1:
			var b [3]byte
			// crypto/rand.Read never returns an error.
			_, _ = rand.Read(b[:])
			name = base + "-" + hex.EncodeToString(b[:])
		case i > 1:
			name = fmt.Sprintf("%s-%d", base, i)
		}
		var user User
		err := tx.QueryRow(ctx, `
			INSERT INTO users (username, display_name, email, quota_bytes) VALUES ($1, $2, $3, $4)
			ON CONFLICT (username) DO NOTHING
			RETURNING id, username, display_name, is_admin`,
			name, display, email, quota).Scan(&user.ID, &user.Username, &user.DisplayName, &user.IsAdmin)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return User{}, fmt.Errorf("create user: %w", err)
		}
		return user, nil
	}
	return User{}, fmt.Errorf("create user: no free username for %q", base)
}

// maxUsernameBase leaves room for a "-xxxxxx" suffix within validUsername's
// 64 characters.
const maxUsernameBase = 56

// usernameBase derives a valid username from the claims: the provider's
// username, else the email's local part, else "user". Characters outside
// validUsername become dashes, so "Vasily Ivanov" gives "Vasily-Ivanov";
// a name with nothing usable (all Cyrillic, say) falls through to the
// next source.
func usernameBase(c Claims) string {
	local, _, _ := strings.Cut(c.Email, "@")
	for _, src := range []string{c.PreferredUsername, local} {
		var b strings.Builder
		dash := false
		for _, r := range src {
			ok := r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
			if !ok || r == '-' {
				if !dash && b.Len() > 0 {
					b.WriteByte('-')
				}
				dash = true
				continue
			}
			// Leading dots and underscores would fail validUsername too.
			if b.Len() == 0 && (r == '.' || r == '_') {
				continue
			}
			b.WriteRune(r)
			dash = false
		}
		name := strings.TrimRight(b.String(), "-")
		if len(name) > maxUsernameBase {
			name = strings.TrimRight(name[:maxUsernameBase], "-")
		}
		if name != "" && validUsername.MatchString(name) {
			return name
		}
	}
	return "user"
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
