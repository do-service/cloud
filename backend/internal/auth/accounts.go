package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Accounts as the admin manages them (R15).

var (
	ErrNoUser = errors.New("auth: no such user")
	// ErrLastAdmin: the change would leave no active administrator.
	ErrLastAdmin = errors.New("auth: the last active administrator can't be disabled, demoted or deleted")
	// ErrInvalidStatus: a status other than active or disabled.
	ErrInvalidStatus = errors.New("auth: status must be active or disabled")
)

// Account statuses; deleting is set by MarkDeleting only.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusDeleting = "deleting"
)

// Account is a user as the admin sees them.
type Account struct {
	ID          string
	Username    string
	DisplayName string
	Email       string
	IsAdmin     bool
	Status      string
	// QuotaBytes is nil when unlimited.
	QuotaBytes *int64
	UsedBytes  int64
	// HasPassword: signs in with a password, not only through a provider.
	HasPassword bool
	// Providers are the sign-in providers the user has identities with.
	Providers  []string
	CreatedAt  time.Time
	LastSeenAt *time.Time
}

const accountColumns = `
	u.id::text, u.username::text, u.display_name, coalesce(u.email::text, ''), u.is_admin, u.status,
	u.quota_bytes, u.used_bytes, u.password_hash IS NOT NULL,
	coalesce((SELECT array_agg(p.slug ORDER BY p.slug) FROM identities i JOIN auth_providers p ON p.id = i.provider_id WHERE i.user_id = u.id), '{}'),
	u.created_at, (SELECT max(last_seen_at) FROM sessions WHERE user_id = u.id)`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Username, &a.DisplayName, &a.Email, &a.IsAdmin, &a.Status,
		&a.QuotaBytes, &a.UsedBytes, &a.HasPassword, &a.Providers, &a.CreatedAt, &a.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNoUser
	}
	return a, err
}

// Accounts lists users by username, those matching query (part of the
// username, name or email) if given, up to limit after cursor (a
// username, "" from the start); next is the following page's cursor.
func (u *Users) Accounts(ctx context.Context, query, cursor string, limit int) (accounts []Account, next string, err error) {
	pattern := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(query)) + "%"
	rows, err := u.db.Query(ctx, `SELECT `+accountColumns+` FROM users u
		WHERE u.username > $1::citext AND (u.username ILIKE $2 OR u.display_name ILIKE $2 OR u.email ILIKE $2)
		ORDER BY u.username LIMIT $3`, cursor, pattern, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list users: %w", err)
	}
	accounts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Account, error) { return scanAccount(row) })
	if err != nil {
		return nil, "", fmt.Errorf("list users: %w", err)
	}
	if len(accounts) > limit {
		accounts = accounts[:limit]
		next = accounts[limit-1].Username
	}
	return accounts, next, nil
}

// Account returns one user.
func (u *Users) Account(ctx context.Context, id string) (Account, error) {
	if !validUUID(id) {
		return Account{}, ErrNoUser
	}
	return scanAccount(u.db.QueryRow(ctx, `SELECT `+accountColumns+` FROM users u WHERE u.id = $1`, id))
}

// AccountChange is what an admin changes; nil fields stay.
type AccountChange struct {
	DisplayName *string
	IsAdmin     *bool
	Status      *string
	// Quota set changes the quota; a nil inside means unlimited.
	Quota    **int64
	Password *string
}

// UpdateAccount applies c to user id. Disabling or demoting the last
// active administrator is refused. Disabling and setting a password end
// the user's sessions.
func (u *Users) UpdateAccount(ctx context.Context, id string, c AccountChange) (Account, error) {
	if !validUUID(id) {
		return Account{}, ErrNoUser
	}
	if c.Status != nil && *c.Status != StatusActive && *c.Status != StatusDisabled {
		return Account{}, ErrInvalidStatus
	}
	if c.DisplayName != nil && strings.TrimSpace(*c.DisplayName) == "" {
		return Account{}, ErrInvalidUsername
	}
	var hash *string
	if c.Password != nil {
		if len(*c.Password) < minPassword || len(*c.Password) > maxPassword {
			return Account{}, ErrWeakPassword
		}
		h, err := hashPassword(ctx, *c.Password)
		if err != nil {
			return Account{}, err
		}
		hash = &h
	}
	err := u.inTx(ctx, func(tx pgx.Tx) error {
		var isAdmin bool
		var status string
		err := tx.QueryRow(ctx, `SELECT is_admin, status FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&isAdmin, &status)
		if errors.Is(err, pgx.ErrNoRows) || status == StatusDeleting {
			return ErrNoUser
		}
		if err != nil {
			return err
		}
		losesAdmin := isAdmin && status == StatusActive &&
			(c.IsAdmin != nil && !*c.IsAdmin || c.Status != nil && *c.Status != StatusActive)
		if losesAdmin {
			if err := otherAdmin(ctx, tx, id); err != nil {
				return err
			}
		}
		set, args := []string{}, []any{id}
		add := func(col string, v any) {
			args = append(args, v)
			set = append(set, fmt.Sprintf("%s = $%d", col, len(args)))
		}
		if c.DisplayName != nil {
			add("display_name", strings.TrimSpace(*c.DisplayName))
		}
		if c.IsAdmin != nil {
			add("is_admin", *c.IsAdmin)
		}
		if c.Status != nil {
			add("status", *c.Status)
		}
		if c.Quota != nil {
			add("quota_bytes", *c.Quota)
		}
		if hash != nil {
			add("password_hash", *hash)
		}
		if len(set) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE users SET `+strings.Join(set, ", ")+` WHERE id = $1`, args...); err != nil {
				return err
			}
		}
		if c.Status != nil && *c.Status != StatusActive || hash != nil {
			_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, id)
			return err
		}
		return nil
	})
	if err != nil {
		return Account{}, err
	}
	return u.Account(ctx, id)
}

// MarkDeleting signs user id out and keeps them out while their files are
// removed; DeleteAccount finishes. The last active admin is refused.
func (u *Users) MarkDeleting(ctx context.Context, id string) (Account, error) {
	a, err := u.Account(ctx, id)
	if err != nil {
		return Account{}, err
	}
	err = u.inTx(ctx, func(tx pgx.Tx) error {
		var isAdmin bool
		var status string
		if err := tx.QueryRow(ctx, `SELECT is_admin, status FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&isAdmin, &status); err != nil {
			return err
		}
		if isAdmin && status == StatusActive {
			if err := otherAdmin(ctx, tx, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET status = 'deleting' WHERE id = $1`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, id)
		return err
	})
	return a, err
}

// otherAdmin fails with ErrLastAdmin unless an active administrator other
// than id exists. Their rows are locked, so two admins can't demote each
// other at once and leave none.
func otherAdmin(ctx context.Context, tx pgx.Tx, id string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM users WHERE is_admin AND status = 'active' AND id <> $1 ORDER BY id FOR UPDATE`, id)
	if err != nil {
		return err
	}
	others, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(others) == 0 {
		return ErrLastAdmin
	}
	return nil
}

func (u *Users) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// validUUID checks the canonical form, so a malformed id is "no such
// user" rather than a database error.
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f', 'A' <= c && c <= 'F':
		default:
			return false
		}
	}
	return true
}
