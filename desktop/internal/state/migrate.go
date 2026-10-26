package state

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Migrations are numbered like the backend's (NNNN_name.sql) but run by
// this small runner, not goose: a client never migrates down (a newer
// database is refused instead), the version fits in SQLite's own
// PRAGMA user_version, which changes in the same transaction as the
// schema, and goose would bring its dependencies into every client binary
// for a loop of twenty lines.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrTooNew means the database was migrated by a newer client: this one
// doesn't know the schema and must not write to it.
var ErrTooNew = errors.New("state: database is from a newer client version")

type migration struct {
	version int
	name    string
	sql     string
}

func migrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("state: bad migration name %q", e.Name())
		}
		body, err := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("state: migrations not numbered 1..n: %s", m.name)
		}
	}
	return ms, nil
}

// migrate applies what's missing, each migration in its own transaction
// together with the version bump, so a crash leaves the database at one
// version or the next, never between.
func migrate(ctx context.Context, db *sql.DB) error {
	ms, err := migrations()
	if err != nil {
		return err
	}
	for {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("state: migrate: %w", err)
		}
		// Read under the write lock (BEGIN IMMEDIATE): a second process
		// opening the same file at once waits instead of applying twice.
		var v int
		if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
			tx.Rollback()
			return fmt.Errorf("state: migrate: %w", err)
		}
		if v > len(ms) {
			tx.Rollback()
			return fmt.Errorf("%w (schema %d, known %d)", ErrTooNew, v, len(ms))
		}
		if v == len(ms) {
			return tx.Rollback()
		}
		m := ms[v]
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("state: migration %s: %w", m.name, err)
		}
		// PRAGMA takes no parameters; the version is our own integer.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
			tx.Rollback()
			return fmt.Errorf("state: migration %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("state: migration %s: %w", m.name, err)
		}
	}
}
