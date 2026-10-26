// Package state is the registry of connections and each connection's
// database (docs/desktop-client.md): the account, the mirror of the
// server's trees (remote), the last local scan (local), what both sides
// last agreed on (base), selective-sync rules, the ops journal with
// resumable transfers, problems and activity.
//
// SQLite through modernc.org/sqlite, which is pure Go, so the engine
// builds and tests with CGO_ENABLED=0 on every OS. WAL, so the window can
// read while the engine writes; synchronous=FULL, so a committed op intent
// survives a power cut before its action runs (NORMAL would only survive a
// crash of the process). Every write transaction is BEGIN IMMEDIATE: it
// takes the write lock up front and waits for it (busy timeout) instead of
// failing with SQLITE_BUSY when a read transaction tries to upgrade.
//
// SQL is written by hand, as in the backend's repo.go; no ORM.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by the single-row getters.
var ErrNotFound = errors.New("state: not found")

// busyTimeoutMS is how long a writer waits for another one. Long, because
// the only other writers are this process's own goroutines (or a second
// instance, which shouldn't run) and failing a write is worse than waiting.
const busyTimeoutMS = 30_000

// DB is an open state database. It is safe for concurrent use.
type DB struct {
	db   *sql.DB
	path string
	// wmu queues this process's writers in Go: contending for SQLite's
	// write lock instead, they'd sleep in its busy handler (up to 100 ms
	// a try), which with parallel transfers cost more than the writes.
	wmu sync.Mutex
}

// Open opens (creating if needed) the database at path and brings its
// schema up to date.
func Open(ctx context.Context, path string) (*DB, error) {
	// Without the "file:" prefix the driver takes everything before '?' as
	// the file name, so a '?' in the path would be cut off silently.
	if strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("state: path %q contains '?'", path)
	}
	dsn := path + "?_txlock=immediate" +
		fmt.Sprintf("&_pragma=busy_timeout(%d)", busyTimeoutMS) +
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("state: open: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db: db, path: path}, nil
}

func (d *DB) Close() error { return d.db.Close() }

// Path is the database file.
func (d *DB) Path() string { return d.path }

// Update runs fn in a write transaction, committed if fn returns nil.
func (d *DB) Update(ctx context.Context, fn func(*Tx) error) error {
	return d.run(ctx, false, fn)
}

// View runs fn in a read-only transaction: a consistent snapshot that
// doesn't block writers.
func (d *DB) View(ctx context.Context, fn func(*Tx) error) error {
	return d.run(ctx, true, fn)
}

func (d *DB) run(ctx context.Context, readOnly bool, fn func(*Tx) error) error {
	if !readOnly {
		d.wmu.Lock()
		defer d.wmu.Unlock()
	}
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return fmt.Errorf("state: begin: %w", err)
	}
	if err := fn(&Tx{tx: tx, ctx: ctx}); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit: %w", err)
	}
	return nil
}

// IntegrityCheck runs PRAGMA integrity_check: nil when SQLite finds the
// file sound.
func (d *DB) IntegrityCheck(ctx context.Context) error {
	rows, err := d.db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("state: integrity_check: %w", err)
	}
	defer rows.Close()
	var msgs []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return fmt.Errorf("state: integrity_check: %w", err)
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("state: integrity_check: %w", err)
	}
	if len(msgs) != 1 || msgs[0] != "ok" {
		return fmt.Errorf("state: integrity_check: %s", strings.Join(msgs, "; "))
	}
	return nil
}

// Tx is a transaction of Update or View; it is only valid inside fn.
type Tx struct {
	tx  *sql.Tx
	ctx context.Context
}

func (t *Tx) exec(query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(t.ctx, query, args...)
}

func (t *Tx) query(query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(t.ctx, query, args...)
}

func (t *Tx) queryRow(query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(t.ctx, query, args...)
}

// notFound turns sql.ErrNoRows into ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// collect scans every row with scan; it closes rows.
func collect[T any](rows *sql.Rows, err error, scan func(*sql.Rows) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// each calls fn for every row, for tables too big to hold twice in memory
// (remote, local, base on a large tree).
func each[T any](rows *sql.Rows, err error, scan func(*sql.Rows) (T, error), fn func(T) error) error {
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return err
		}
		if err := fn(v); err != nil {
			return err
		}
	}
	return rows.Err()
}
