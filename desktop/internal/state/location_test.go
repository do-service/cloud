package state

import (
	"path/filepath"
	"testing"
)

func TestBaseDirFor(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		goos, home string
		env        map[string]string
		want       string
	}{
		{"darwin", "/Users/v", nil, "/Users/v/Library/Application Support/Cloud"},
		{"windows", `C:\Users\v`, map[string]string{"LOCALAPPDATA": `C:\Users\v\AppData\Local`}, `C:\Users\v\AppData\Local\Cloud`},
		{"windows", `C:\Users\v`, nil, `C:\Users\v\AppData\Local\Cloud`},
		{"linux", "/home/v", map[string]string{"XDG_STATE_HOME": "/var/state"}, "/var/state/cloud"},
		{"linux", "/home/v", nil, "/home/v/.local/state/cloud"},
		// A relative XDG_STATE_HOME is invalid by the spec and ignored.
		{"freebsd", "/home/v", map[string]string{"XDG_STATE_HOME": "rel"}, "/home/v/.local/state/cloud"},
	}
	for _, c := range cases {
		got, err := baseDirFor(c.goos, env(c.env), c.home)
		if err != nil || got != c.want {
			t.Errorf("%s %v: %q, %v; want %q", c.goos, c.env, got, err, c.want)
		}
	}
	if _, err := baseDirFor("darwin", env(nil), ""); err == nil {
		t.Error("darwin without a home accepted")
	}
}

func TestDir(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "c:"} {
		if _, err := Dir(bad); err == nil {
			t.Errorf("connection id %q accepted", bad)
		}
	}
	base, err := BaseDir()
	if err != nil {
		t.Skip("no base dir here:", err)
	}
	if p, err := Path("abc"); err != nil || p != filepath.Join(base, "abc", FileName) {
		t.Fatalf("Path = %q, %v", p, err)
	}
}
