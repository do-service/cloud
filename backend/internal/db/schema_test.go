package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// schemaTx migrates the test database and returns a transaction that is
// rolled back after the test, so schema tests leave no rows behind.
func schemaTx(t *testing.T) pgx.Tx {
	t.Helper()
	url := testDatabaseURL()
	if err := Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	pool, err := NewPool(ctx, url, 2, 0, 0)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// savepoint runs one statement that may fail without aborting the test's
// transaction, returning the Postgres error code ("" on success).
func savepoint(t *testing.T, tx pgx.Tx, sql string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	_, err = sp.Exec(ctx, sql, args...)
	if err == nil {
		if err := sp.Commit(ctx); err != nil {
			t.Fatalf("release savepoint: %v", err)
		}
		return ""
	}
	_ = sp.Rollback(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("exec %q: %v", sql, err)
	}
	return pgErr.Code
}

const (
	codeUniqueViolation = "23505"
	codeCheckViolation  = "23514"
)

type fixture struct {
	tx     pgx.Tx
	userID string
	rootID string
	blobID string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	tx := schemaTx(t)
	ctx := context.Background()
	f := fixture{tx: tx}
	if err := tx.QueryRow(ctx, `INSERT INTO users (username) VALUES ('alice') RETURNING id`).Scan(&f.userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO nodes (owner_id, name, name_key, kind) VALUES ($1, '', '', 'dir') RETURNING id`, f.userID).Scan(&f.rootID); err != nil {
		t.Fatalf("insert root: %v", err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO blobs (backend, size, hash) VALUES ('localfs', 0, sha256(''::bytea)) RETURNING id`).Scan(&f.blobID); err != nil {
		t.Fatalf("insert blob: %v", err)
	}
	return f
}

func (f fixture) insertDir(t *testing.T, name, key string) string {
	t.Helper()
	return savepoint(t, f.tx, `INSERT INTO nodes (owner_id, parent_id, name, name_key, kind) VALUES ($1, $2, $3, $4, 'dir')`,
		f.userID, f.rootID, name, key)
}

func TestNodesNameUniqueness(t *testing.T) {
	f := newFixture(t)
	if code := f.insertDir(t, "Docs", "docs"); code != "" {
		t.Fatalf("first insert: %s", code)
	}
	// Different spelling, same key: the fs service maps both to "docs".
	if code := f.insertDir(t, "DOCS", "docs"); code != codeUniqueViolation {
		t.Fatalf("duplicate name_key: code %q, want unique violation", code)
	}
	// A trashed node no longer holds its name.
	if _, err := f.tx.Exec(context.Background(), `UPDATE nodes SET trashed_at = now(), trash_root = true WHERE name_key = 'docs'`); err != nil {
		t.Fatalf("trash: %v", err)
	}
	if code := f.insertDir(t, "docs", "docs"); code != "" {
		t.Fatalf("insert after trashing: %s", code)
	}
}

func TestNodesConstraints(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name string
		sql  string
		args []any
		want string
	}{
		{"second root", `INSERT INTO nodes (owner_id, name, name_key, kind) VALUES ($1, '', '', 'dir')`,
			[]any{f.userID}, codeUniqueViolation},
		{"file root", `INSERT INTO nodes (owner_id, name, name_key, kind, blob_id) VALUES ($1, '', '', 'file', $2)`,
			[]any{f.userID, f.blobID}, codeCheckViolation},
		{"nameless child", `INSERT INTO nodes (owner_id, parent_id, name, name_key, kind) VALUES ($1, $2, '', '', 'dir')`,
			[]any{f.userID, f.rootID}, codeCheckViolation},
		{"file without blob", `INSERT INTO nodes (owner_id, parent_id, name, name_key, kind) VALUES ($1, $2, 'a', 'a', 'file')`,
			[]any{f.userID, f.rootID}, codeCheckViolation},
		{"dir with blob", `INSERT INTO nodes (owner_id, parent_id, name, name_key, kind, blob_id) VALUES ($1, $2, 'a', 'a', 'dir', $3)`,
			[]any{f.userID, f.rootID, f.blobID}, codeCheckViolation},
		{"trash root not trashed", `INSERT INTO nodes (owner_id, parent_id, name, name_key, kind, trash_root) VALUES ($1, $2, 'a', 'a', 'dir', true)`,
			[]any{f.userID, f.rootID}, codeCheckViolation},
		{"s3 blob without key", `INSERT INTO blobs (backend, size, hash) VALUES ('s3', 0, sha256(''::bytea))`,
			nil, codeCheckViolation},
		{"file", `INSERT INTO nodes (owner_id, parent_id, name, name_key, kind, blob_id) VALUES ($1, $2, 'a', 'a', 'file', $3)`,
			[]any{f.userID, f.rootID, f.blobID}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := savepoint(t, f.tx, tc.sql, tc.args...); code != tc.want {
				t.Fatalf("code %q, want %q", code, tc.want)
			}
		})
	}
}

func TestNameOrder(t *testing.T) {
	f := newFixture(t)
	// Inserted shuffled; name_key is already lower-case, as the fs service
	// stores it.
	want := []string{"file2", "file10", "zeta", "б", "ё", "ж", "файл9", "файл10", "я"}
	for _, k := range []string{"я", "файл10", "file10", "ж", "ё", "zeta", "файл9", "б", "file2"} {
		if code := f.insertDir(t, k, k); code != "" {
			t.Fatalf("insert %q: %s", k, code)
		}
	}
	rows, err := f.tx.Query(context.Background(),
		`SELECT name_key FROM nodes WHERE parent_id = $1 ORDER BY kind, name_key, id`, f.rootID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %q, want %q", got, want)
	}
}

func TestNodesCascadeAndSearch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var dirID string
	if err := f.tx.QueryRow(ctx, `INSERT INTO nodes (owner_id, parent_id, name, name_key, kind) VALUES ($1, $2, 'Отчёты', 'отчёты', 'dir') RETURNING id`,
		f.userID, f.rootID).Scan(&dirID); err != nil {
		t.Fatalf("insert dir: %v", err)
	}
	var fileID string
	if err := f.tx.QueryRow(ctx, `INSERT INTO nodes (owner_id, parent_id, name, name_key, kind, blob_id) VALUES ($1, $2, 'Квартал 3.xlsx', 'квартал 3.xlsx', 'file', $3) RETURNING id`,
		f.userID, dirID, f.blobID).Scan(&fileID); err != nil {
		t.Fatalf("insert file: %v", err)
	}
	if _, err := f.tx.Exec(ctx, `INSERT INTO versions (node_id, blob_id, size, hash, author_id) VALUES ($1, $2, 0, sha256(''::bytea), $3)`,
		fileID, f.blobID, f.userID); err != nil {
		t.Fatalf("insert version: %v", err)
	}

	var found string
	if err := f.tx.QueryRow(ctx, `SELECT name FROM nodes WHERE name_key ILIKE '%' || $1 || '%' AND trashed_at IS NULL`, "ртал").Scan(&found); err != nil {
		t.Fatalf("search: %v", err)
	}
	if found != "Квартал 3.xlsx" {
		t.Fatalf("search found %q", found)
	}

	// Purging a folder takes its subtree and the subtree's versions along.
	if _, err := f.tx.Exec(ctx, `DELETE FROM nodes WHERE id = $1`, dirID); err != nil {
		t.Fatalf("delete dir: %v", err)
	}
	var left int
	if err := f.tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM nodes WHERE id = $1) + (SELECT count(*) FROM versions WHERE node_id = $1)`, fileID).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 0 {
		t.Fatalf("%d rows survived the cascade", left)
	}
}
