package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/config"
	"github.com/do-service/cloud/backend/internal/db"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/storage/localfs"
)

func TestBootstrapAdmin(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
	}
	if err := db.Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, url, 4, 0, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE users CASCADE`); err != nil {
		t.Fatal(err)
	}
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	users, files := auth.NewUsers(pool), fs.New(pool, store)

	count := func() (users, roots int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM nodes WHERE parent_id IS NULL)`).Scan(&users, &roots); err != nil {
			t.Fatal(err)
		}
		return users, roots
	}

	// Without the variables nothing is created.
	if err := bootstrapAdmin(ctx, config.Config{}, users, files); err != nil {
		t.Fatalf("no env: %v", err)
	}
	if u, _ := count(); u != 0 {
		t.Fatalf("%d users created without env", u)
	}

	cfg := config.Config{AdminUsername: "admin", AdminPassword: "first-password"}
	if err := bootstrapAdmin(ctx, cfg, users, files); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if u, r := count(); u != 1 || r != 1 {
		t.Fatalf("after bootstrap: %d users, %d roots", u, r)
	}
	if u, err := users.Authenticate(ctx, "admin", "first-password"); err != nil || !u.IsAdmin {
		t.Fatalf("admin login: %+v, %v", u, err)
	}

	// Restarting with changed variables neither adds an account nor resets
	// the existing password.
	cfg = config.Config{AdminUsername: "root", AdminPassword: "other-password"}
	if err := bootstrapAdmin(ctx, cfg, users, files); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if u, _ := count(); u != 1 {
		t.Fatalf("second bootstrap created a user: %d", u)
	}
}
