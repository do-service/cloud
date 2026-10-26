package audit

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/do-service/cloud/backend/internal/db"
)

func testDatabaseURL() string {
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
}

func TestRecordListPrune(t *testing.T) {
	url := testDatabaseURL()
	if err := db.Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, url, 4, 0, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE audit_log`); err != nil {
		t.Fatal(err)
	}
	l := New(pool)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "[::ffff:192.0.2.7]:5555"
	alice := Actor{ID: "11111111-1111-4111-8111-111111111111", Name: "alice"}
	l.Record(r, Event{Actor: alice, Action: Login, Details: map[string]any{"method": "password"}})
	l.Record(r, Event{Actor: Actor{Name: "mallory"}, Action: LoginFailed})
	for i := range 3 {
		l.Record(r, Event{Actor: alice, Action: UserUpdate, TargetID: "u", TargetName: "bob", Details: map[string]any{"n": i}})
	}
	// A nil log records nothing, and doesn't fail.
	var none *Log
	none.Record(r, Event{Action: Login})

	all, total, err := l.List(ctx, Filter{}, 0, 10)
	if err != nil || len(all) != 5 || total != 5 {
		t.Fatalf("list: %d entries, total %d, %v", len(all), total, err)
	}
	newest := all[0]
	if newest.Action != UserUpdate || newest.Details["n"] != 2.0 || newest.IP != "192.0.2.7" || newest.Actor != alice || newest.TargetName != "bob" {
		t.Fatalf("newest = %+v", newest)
	}
	if failed := all[3]; failed.Actor.ID != "" || failed.Actor.Name != "mallory" {
		t.Fatalf("failed login = %+v", failed)
	}

	page, total, err := l.List(ctx, Filter{Action: "admin."}, 0, 2)
	if err != nil || len(page) != 2 || total != 3 {
		t.Fatalf("first page: %d, %d, %v", len(page), total, err)
	}
	rest, total, err := l.List(ctx, Filter{Action: "admin."}, 2, 2)
	if err != nil || len(rest) != 1 || total != 3 || rest[0].Details["n"] != 0.0 {
		t.Fatalf("second page: %+v, %d, %v", rest, total, err)
	}
	if past, total, err := l.List(ctx, Filter{Action: "admin."}, 10, 2); err != nil || len(past) != 0 || total != 3 {
		t.Fatalf("past the end: %d, %d, %v", len(past), total, err)
	}
	if mine, _, err := l.List(ctx, Filter{Actor: "ALICE", Action: Login}, 0, 10); err != nil || len(mine) != 1 {
		t.Fatalf("by actor and action: %d, %v", len(mine), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_log SET at = now() - interval '2 days' WHERE action = $1`, LoginFailed); err != nil {
		t.Fatal(err)
	}
	r.RemoteAddr = "198.51.100.1:5555"
	l.Record(r, Event{Actor: alice, Action: NodeTrash, TargetID: "n1", TargetName: `100%_a\b.txt`})
	l.Record(r, Event{Actor: alice, Action: NodeTrash, TargetID: "n2", TargetName: "100x-a-b.txt"})
	day := time.Now().Add(-24 * time.Hour)
	for _, tc := range []struct {
		name string
		f    Filter
		want int
	}{
		// The failed sign-in names no account: found by its login all the same.
		{"unknown actor", Filter{Actor: "Mallory"}, 1},
		{"from", Filter{From: day}, 6},
		{"to", Filter{To: day}, 1},
		{"from and to", Filter{From: day.Add(-48 * time.Hour), To: day}, 1},
		{"target name, any case", Filter{Query: "BOB"}, 3},
		// % and _ are literal, not wildcards.
		{"target name, wildcards", Filter{Query: `0%_a\b`}, 1},
		{"target name, no wildcard match", Filter{Query: "0%_"}, 1},
		{"target id", Filter{Query: "n2"}, 1},
		{"target id is exact", Filter{Query: "n"}, 0},
		{"ip", Filter{IP: netip.MustParsePrefix("192.0.2.7/32")}, 5},
		{"network", Filter{IP: netip.MustParsePrefix("198.51.100.0/24")}, 2},
		{"all at once", Filter{Action: "node.", Actor: "alice", Query: "txt", IP: netip.MustParsePrefix("198.51.0.0/16"), From: day}, 2},
	} {
		if got, total, err := l.List(ctx, tc.f, 0, 10); err != nil || len(got) != tc.want || total != int64(tc.want) {
			t.Errorf("%s: %d entries, total %d, %v; want %d", tc.name, len(got), total, err, tc.want)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE audit_log SET at = now() - interval '400 days' WHERE action = $1`, Login); err != nil {
		t.Fatal(err)
	}
	if n, err := l.Prune(ctx, time.Now().Add(-365*24*time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("prune: %d, %v", n, err)
	}
}
