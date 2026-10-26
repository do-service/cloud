package access

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/db"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
	}
	if err := db.Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	pool, err := db.NewPool(context.Background(), url, 4, 0, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `TRUNCATE users, nodes, blobs, versions, groups CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestPerms(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	id := func(sql string, args ...any) string {
		t.Helper()
		var v string
		if err := pool.QueryRow(ctx, sql+` RETURNING id::text`, args...).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	user := func(name string) string { return id(`INSERT INTO users (username) VALUES ($1)`, name) }
	alice, bob, carol, dave := user("alice"), user("bob"), user("carol"), user("dave")
	dir := func(parent *string, name string) string {
		return id(`INSERT INTO nodes (owner_id, parent_id, name, name_key, kind) VALUES ($1, $2, $3, lower($3), 'dir')`, alice, parent, name)
	}
	// alice's tree: root / A / B / C
	root := id(`INSERT INTO nodes (owner_id, name, name_key, kind) VALUES ($1, '', '', 'dir')`, alice)
	a := dir(&root, "A")
	b := dir(&a, "B")
	c := dir(&b, "C")
	team := id(`INSERT INTO groups (name) VALUES ('team')`)
	if _, err := pool.Exec(ctx, `INSERT INTO group_members VALUES ($1, $2)`, team, carol); err != nil {
		t.Fatal(err)
	}
	share := func(node, typ, grantee string, p Perm) string {
		return id(`INSERT INTO shares (node_id, grantee_type, grantee_id, perms) VALUES ($1, $2, $3, $4)`, node, typ, grantee, int16(p))
	}
	share(b, "user", bob, Read)
	share(a, "user", bob, Read|Reshare) // rights add up along the path
	teamShare := share(b, "group", team, Read|Write)

	check := func(label, user, node string, want Perm) {
		t.Helper()
		got, err := Perms(ctx, pool, user, node, alice)
		if err != nil || got != want {
			t.Errorf("%s: %v, %v; want %v", label, got, err, want)
		}
	}
	check("owner anywhere", alice, c, All)
	check("bob above any share", bob, root, None)
	check("bob on A", bob, a, Read|Reshare)
	check("bob on C, inherited from A and B", bob, c, Read|Reshare)
	check("carol through her group, below B", carol, c, Read|Write)
	check("carol above the group's share", carol, a, None)
	check("dave, no share", dave, c, None)

	// Revoking and leaving a group act at once: nothing is cached.
	if _, err := pool.Exec(ctx, `DELETE FROM shares WHERE id = $1`, teamShare); err != nil {
		t.Fatal(err)
	}
	check("carol after revoke", carol, c, None)
	share(b, "group", team, Read)
	if _, err := pool.Exec(ctx, `DELETE FROM group_members WHERE user_id = $1`, carol); err != nil {
		t.Fatal(err)
	}
	check("carol after leaving the group", carol, c, None)

	roots, err := ShareRoots(ctx, pool, bob, []string{root, a, b, c})
	if err != nil || len(roots) != 2 || !roots[a] || !roots[b] {
		t.Fatalf("share roots for bob: %v, %v", roots, err)
	}
}
