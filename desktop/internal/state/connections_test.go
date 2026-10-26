package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeServer(t *testing.T) {
	ok := map[string]string{
		"https://Cloud.Example.RU/":        "https://cloud.example.ru",
		"HTTPS://cloud.example.ru:443":     "https://cloud.example.ru",
		"https://cloud.example.ru:8443/x/": "https://cloud.example.ru:8443/x",
		"http://localhost:8090":            "http://localhost:8090",
		"http://127.0.0.1:80/":             "http://127.0.0.1",
		"http://[::1]:8090":                "http://[::1]:8090",
		"https://[2001:DB8::1]":            "https://[2001:db8::1]",
		" https://cloud.example.ru ":       "https://cloud.example.ru",
	}
	for in, want := range ok {
		if got, err := NormalizeServer(in); err != nil || got != want {
			t.Errorf("NormalizeServer(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "cloud.example.ru", "ftp://x", "http://cloud.example.ru", "https://u:p@x", "https://x/?a=1", "https://x/#f"} {
		if got, err := NormalizeServer(bad); err == nil {
			t.Errorf("NormalizeServer(%q) = %q, want an error", bad, got)
		}
	}
}

func TestNested(t *testing.T) {
	cases := []struct {
		a, b     string
		fold     bool
		expected bool
	}{
		{"/Users/v/Cloud/a", "/Users/v/Cloud/b", false, false},
		{"/Users/v/Cloud", "/Users/v/Cloud/b", false, true},
		{"/Users/v/Cloud/b/", "/Users/v/Cloud", false, true},
		{"/Users/v/Cloud", "/Users/v/Cloud", false, true},
		// A common prefix of names is not nesting.
		{"/Users/v/Cloud", "/Users/v/Cloud2", false, false},
		{"/Users/v/cloud", "/Users/v/Cloud/x", true, true},
		{"/Users/v/cloud", "/Users/v/Cloud/x", false, false},
		// NFD and NFC spellings are one folder.
		{"/Users/v/Облако/Café", "/Users/v/Облако/Café/sub", false, true},
		{"/Users/v/a/../Cloud", "/Users/v/Cloud/x", false, true},
	}
	for _, c := range cases {
		if got := Nested(c.a, c.b, c.fold); got != c.expected {
			t.Errorf("Nested(%q, %q, %v) = %v", c.a, c.b, c.fold, got)
		}
	}
}

func TestRegistry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Cloud")
	r := OpenRegistry(dir)
	if conns, err := r.List(); err != nil || len(conns) != 0 {
		t.Fatalf("empty List() = %v, %v", conns, err)
	}
	a, err := r.Add(Connection{Server: "https://Cloud.Example.ru/", UserID: "u1", UserName: "Василий", LocalRoot: "/Users/v/Cloud/cloud.example.ru"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Server != "https://cloud.example.ru" || a.CreatedAt.IsZero() || !validID(a.ID) {
		t.Fatalf("Add = %+v", a)
	}
	// Another user on the same server, another server: fine.
	b, err := r.Add(Connection{Server: "https://cloud.example.ru", UserID: "u2", LocalRoot: "/Users/v/Cloud/u2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(Connection{Server: "https://cloud.example.ru:443", UserID: "u1", LocalRoot: "/Users/v/Other"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("same account again: %v", err)
	}
	if _, err := r.Add(Connection{Server: "https://other.example", UserID: "u1", LocalRoot: "/Users/v/Cloud/u2/inner"}); !errors.Is(err, ErrNested) {
		t.Fatalf("nested folder: %v", err)
	}
	if _, err := r.Add(Connection{Server: "https://other.example", UserID: "u1", LocalRoot: "relative"}); err == nil {
		t.Fatal("relative folder accepted")
	}
	if err := r.SetLocalRoot(b.ID, "/Users/v/Cloud"); !errors.Is(err, ErrNested) {
		t.Fatalf("SetLocalRoot to a parent of another: %v", err)
	}
	if err := r.SetLocalRoot(b.ID, "/Users/v/Cloud2"); err != nil {
		t.Fatal(err)
	}

	// A fresh registry reads what the first one wrote.
	conns, err := OpenRegistry(dir).List()
	if err != nil || len(conns) != 2 || conns[0] != a || conns[1].LocalRoot != "/Users/v/Cloud2" {
		t.Fatalf("List() = %+v, %v", conns, err)
	}
	if err := r.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Remove: %v", err)
	}
	if _, err := r.Get(b.ID); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != RegistryFile {
		t.Fatalf("left in %s: %v", dir, entries)
	}
}
