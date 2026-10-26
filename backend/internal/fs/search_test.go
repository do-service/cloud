package fs

import (
	"context"
	"errors"
	"slices"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func TestSearch(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "Docs")
	sub := e.mkdir(t, docs.ID, "sub")
	e.file(t, docs.ID, "Отчёт 2026.docx", "")
	e.file(t, sub.ID, "отчёт-копия.txt", "")
	e.mkdir(t, e.root.ID, "ОТЧЁТЫ")
	e.file(t, e.root.ID, "50%_off.txt", "")
	e.file(t, e.root.ID, "500 off.txt", "")
	old := e.file(t, sub.ID, "отчёт старый.txt", "")
	e.trash(t, old.ID)
	bob := e.newUser(t, "bob")
	bobRoot, err := e.svc.EnsureRoot(ctx, bob)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateDir(ctx, bob, bobRoot.ID, "отчёт bob"); err != nil {
		t.Fatal(err)
	}

	search := func(q string, limit int) []string {
		t.Helper()
		var got []string
		cursor := ""
		for {
			items, next, err := e.svc.Search(ctx, e.user, q, limit, cursor)
			if err != nil {
				t.Fatalf("search %q: %v", q, err)
			}
			for _, it := range items {
				got = append(got, it.Path+"|"+it.Name)
			}
			if next == "" {
				return got
			}
			cursor = next
		}
	}
	want := []string{"Docs|Отчёт 2026.docx", "Docs/sub|отчёт-копия.txt", "|ОТЧЁТЫ"}
	for _, q := range []string{"отчёт", "  ОТЧЁТ ", norm.NFD.String("отчёт")} {
		if got := search(q, 0); !slices.Equal(got, want) {
			t.Errorf("search %q:\n got %v\nwant %v", q, got, want)
		}
	}
	if got := search("отчёт", 1); !slices.Equal(got, want) {
		t.Errorf("paged by 1: %v", got)
	}
	if got := search("50%_", 0); !slices.Equal(got, []string{"|50%_off.txt"}) {
		t.Errorf("wildcards taken literally: %v", got)
	}
	if got := search("nothing like it", 0); len(got) != 0 {
		t.Errorf("no match: %v", got)
	}

	for _, q := range []string{"", "аб", "  ab  "} {
		if _, _, err := e.svc.Search(ctx, e.user, q, 0, ""); !errors.Is(err, ErrQueryTooShort) {
			t.Errorf("%q: %v", q, err)
		}
	}
	_, next, err := e.svc.Search(ctx, e.user, "отчёт", 1, "")
	if err != nil || next == "" {
		t.Fatal(next, err)
	}
	for _, c := range []string{"!!", next} {
		if _, _, err := e.svc.Search(ctx, e.user, "копия", 1, c); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("cursor %q for another query: %v", c, err)
		}
	}
}
