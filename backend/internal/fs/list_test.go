package fs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// listAll pages through dirID with limit and returns the names in order.
func (e env) listAll(t *testing.T, dirID string, opts ListOptions) []string {
	t.Helper()
	var names []string
	for pages := 0; ; pages++ {
		if pages > 1000 {
			t.Fatal("listing never ends")
		}
		items, next, err := e.svc.List(context.Background(), e.user, dirID, opts)
		if err != nil {
			t.Fatalf("list %+v: %v", opts, err)
		}
		if next != "" && len(items) != opts.Limit {
			t.Fatalf("short page of %d with a next cursor", len(items))
		}
		for _, n := range items {
			names = append(names, n.Name)
		}
		if next == "" {
			return names
		}
		opts.Cursor = next
	}
}

func TestListOrders(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	dir := e.mkdir(t, e.root.ID, "big")
	// Dirs all have size 0 and files repeat sizes and mtimes, so the
	// id tie-break is exercised by every sort.
	for i := range 7 {
		e.mkdir(t, dir.ID, fmt.Sprintf("папка %d", i+1))
	}
	for i := range 20 {
		e.file(t, dir.ID, fmt.Sprintf("file%d.txt", i+1), strings.Repeat("x", i%4))
	}
	if _, err := e.pool.Exec(ctx, `UPDATE nodes SET mtime = '2026-01-01'::timestamptz + (abs(hashtext(name)) % 3) * interval '1 day' WHERE parent_id = $1`, dir.ID); err != nil {
		t.Fatal(err)
	}
	trashed := e.file(t, dir.ID, "trashed.txt", "")
	if _, err := e.pool.Exec(ctx, `UPDATE nodes SET trashed_at = now(), trash_root = true WHERE id = $1`, trashed.ID); err != nil {
		t.Fatal(err)
	}

	for _, sort := range []Sort{SortName, SortMTime, SortSize} {
		for _, desc := range []bool{false, true} {
			// The reference is one unpaginated query with the same order.
			col, dirSQL := sortColumns[sort], "ASC"
			if desc {
				dirSQL = "DESC"
			}
			rows, err := e.pool.Query(ctx, fmt.Sprintf(`SELECT name FROM nodes WHERE parent_id = $1 AND trashed_at IS NULL ORDER BY kind, %s %s, id %s`, col, dirSQL, dirSQL), dir.ID)
			if err != nil {
				t.Fatal(err)
			}
			want, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			for _, limit := range []int{1, 3, 7, 26, 100} {
				got := e.listAll(t, dir.ID, ListOptions{Sort: sort, Desc: desc, Limit: limit})
				if !slices.Equal(got, want) {
					t.Errorf("%s desc=%v limit=%d:\n got %v\nwant %v", sort, desc, limit, got, want)
				}
			}
			if !strings.HasPrefix(want[0], "папка") || !strings.HasPrefix(want[6], "папка") || !strings.HasPrefix(want[7], "file") {
				t.Errorf("%s desc=%v: folders not first: %v", sort, desc, want)
			}
		}
	}
}

func TestListNameOrder(t *testing.T) {
	e := newEnv(t, nil)
	dir := e.mkdir(t, e.root.ID, "names")
	for _, name := range []string{"файл10", "Яблоко", "ёлка", "файл2", "Ежик", "B.txt", "a.txt", "жук"} {
		e.file(t, dir.ID, name, "")
	}
	want := []string{"a.txt", "B.txt", "Ежик", "ёлка", "жук", "файл2", "файл10", "Яблоко"}
	if got := e.listAll(t, dir.ID, ListOptions{Limit: 3}); !slices.Equal(got, want) {
		t.Errorf("name order:\n got %v\nwant %v", got, want)
	}
	slices.Reverse(want)
	if got := e.listAll(t, dir.ID, ListOptions{Desc: true, Limit: 3}); !slices.Equal(got, want) {
		t.Errorf("name order desc:\n got %v\nwant %v", got, want)
	}
}

// A node added before the cursor isn't returned, and nothing after it
// repeats or goes missing.
func TestListStableUnderInserts(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	dir := e.mkdir(t, e.root.ID, "d")
	for _, name := range []string{"b", "d", "f", "h"} {
		e.file(t, dir.ID, name, "")
	}
	page, next, err := e.svc.List(ctx, e.user, dir.ID, ListOptions{Limit: 2})
	if err != nil || len(page) != 2 || next == "" {
		t.Fatalf("first page: %v, %q, %v", page, next, err)
	}
	e.file(t, dir.ID, "a", "")
	e.file(t, dir.ID, "e", "")
	e.mkdir(t, dir.ID, "c-dir")
	got := e.listAll(t, dir.ID, ListOptions{Limit: 2, Cursor: next})
	if want := []string{"e", "f", "h"}; !slices.Equal(got, want) {
		t.Errorf("after inserts: got %v, want %v", got, want)
	}
}

func TestListRejects(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	dir := e.mkdir(t, e.root.ID, "d")
	for _, name := range []string{"a", "b", "c"} {
		e.file(t, dir.ID, name, "x")
	}
	f := e.file(t, e.root.ID, "plain.txt", "")
	_, bySize, err := e.svc.List(ctx, e.user, dir.ID, ListOptions{Sort: SortSize, Limit: 1})
	if err != nil || bySize == "" {
		t.Fatalf("list: %q, %v", bySize, err)
	}
	bob := e.newUser(t, "bob")

	cases := []struct {
		name  string
		actor string
		id    string
		opts  ListOptions
		want  error
	}{
		{"file", e.user, f.ID, ListOptions{}, ErrNotDir},
		{"foreign folder", bob, dir.ID, ListOptions{}, ErrNotFound},
		{"missing", e.user, "00000000-0000-0000-0000-000000000000", ListOptions{}, ErrNotFound},
		{"malformed id", e.user, "nope", ListOptions{}, ErrNotFound},
		{"garbage cursor", e.user, dir.ID, ListOptions{Cursor: "!!"}, ErrInvalidCursor},
		{"cursor of another sort", e.user, dir.ID, ListOptions{Sort: SortMTime, Cursor: bySize}, ErrInvalidCursor},
		{"cursor of another order", e.user, dir.ID, ListOptions{Sort: SortSize, Desc: true, Cursor: bySize}, ErrInvalidCursor},
		{"forged cursor value", e.user, dir.ID, ListOptions{Sort: SortSize, Cursor: encodeCursor(cursor{Sort: SortSize, Kind: KindFile, Value: "big", ID: f.ID})}, ErrInvalidCursor},
	}
	for _, tc := range cases {
		if _, _, err := e.svc.List(ctx, tc.actor, tc.id, tc.opts); !errors.Is(err, tc.want) {
			t.Errorf("%s: err %v, want %v", tc.name, err, tc.want)
		}
	}
	if items, _, err := e.svc.List(ctx, e.user, dir.ID, ListOptions{Limit: MaxListLimit + 1}); err != nil || len(items) != 3 {
		t.Errorf("over-limit: %d items, %v", len(items), err)
	}
}

func TestWalk(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "Docs")
	e.file(t, docs.ID, "a.txt", "a")
	sub := e.mkdir(t, docs.ID, "sub")
	e.file(t, sub.ID, "b.txt", "bb")
	e.mkdir(t, docs.ID, "Пусто")
	e.mkdir(t, docs.ID, "a b")
	gone := e.mkdir(t, docs.ID, "gone")
	e.file(t, gone.ID, "inside.txt", "")
	if _, err := e.pool.Exec(ctx, `UPDATE nodes SET trashed_at = now() WHERE id = $1 OR parent_id = $1`, gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE nodes SET trash_root = true WHERE id = $1`, gone.ID); err != nil {
		t.Fatal(err)
	}

	entries, err := e.svc.Walk(ctx, e.user, docs.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, en := range entries {
		got = append(got, fmt.Sprintf("%s %s %d", en.Kind, en.Path, en.Size))
	}
	want := []string{"dir a b 0", "file a.txt 1", "dir sub 2", "file sub/b.txt 2", "dir Пусто 0"}
	if !slices.Equal(got, want) {
		t.Fatalf("walk:\n got %q\nwant %q", got, want)
	}

	f := e.file(t, e.root.ID, "f", "")
	bob := e.newUser(t, "bob")
	for _, tc := range []struct {
		name, actor, id string
		want            error
	}{
		{"file", e.user, f.ID, ErrNotDir},
		{"foreign", bob, docs.ID, ErrNotFound},
		{"malformed", e.user, "x", ErrNotFound},
	} {
		if _, err := e.svc.Walk(ctx, tc.actor, tc.id); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}
