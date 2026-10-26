package db

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func testDatabaseURL() string {
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
}

// TestMigrationsDownUp checks every migration's Down actually reverses its
// Up (a broken Down only surfaces during an emergency rollback otherwise),
// and that Up re-applies cleanly afterwards.
func TestMigrationsDownUp(t *testing.T) {
	url := testDatabaseURL()
	if err := Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqlDB.Close()

	extensionExists := func(name string) bool {
		t.Helper()
		var exists bool
		if err := sqlDB.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)`, name).Scan(&exists); err != nil {
			t.Fatalf("check extension %s: %v", name, err)
		}
		return exists
	}
	extensions := []string{"pgcrypto", "citext", "pg_trgm", "btree_gin"}

	// Tables and the collation count as "extensions" here: objects that
	// must exist after up and be gone after down.
	objectExists := func(name string) bool {
		t.Helper()
		var exists bool
		if err := sqlDB.QueryRow(`SELECT to_regclass($1) IS NOT NULL OR EXISTS (SELECT 1 FROM pg_collation WHERE collname = $1)`, name).Scan(&exists); err != nil {
			t.Fatalf("check object %s: %v", name, err)
		}
		return exists
	}
	objects := []string{"users", "blobs", "nodes", "versions", "name_order", "sessions", "auth_providers", "identities", "app_passwords", "uploads", "groups", "group_members", "shares", "links", "link_sessions", "dav_props", "locks", "wopi_tokens", "audit_log", "previews"}
	for _, name := range objects {
		if !objectExists(name) {
			t.Errorf("%s missing after up", name)
		}
	}

	for _, name := range extensions {
		if !extensionExists(name) {
			t.Errorf("extension %s missing after up", name)
		}
	}

	if err := goose.DownTo(sqlDB, "migrations", 0); err != nil {
		t.Fatalf("migrate down to 0: %v", err)
	}
	for _, name := range extensions {
		if extensionExists(name) {
			t.Errorf("extension %s still exists after down", name)
		}
	}
	for _, name := range objects {
		if objectExists(name) {
			t.Errorf("%s still exists after down", name)
		}
	}

	if err := goose.Up(sqlDB, "migrations"); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	for _, name := range extensions {
		if !extensionExists(name) {
			t.Errorf("extension %s missing after re-up", name)
		}
	}
}

func TestNewPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, testDatabaseURL(), 2, 0, 3*time.Second)
	if err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	defer pool.Close()

	var timeout string
	if err := pool.QueryRow(ctx, `SHOW statement_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("show statement_timeout: %v", err)
	}
	if timeout != "3s" {
		t.Errorf("statement_timeout = %q, want 3s", timeout)
	}
}
