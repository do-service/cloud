package connection

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/state"
)

const server = "http://localhost:8090"

// testBase is a throwaway HOME and its state directory.
func testBase(t *testing.T) (home, base string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	base, err := state.BaseDir()
	if err != nil {
		t.Fatal(err)
	}
	return home, base
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T, base string, user, dir string) state.Connection {
	t.Helper()
	conn, _, err := Setup(context.Background(), base, signin.Account{Server: server, UserID: user, Username: user, DeviceID: "d-" + user}, dir)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestCheckFolder(t *testing.T) {
	home, base := testBase(t)
	check := func(dir string) (Folder, error) { return CheckFolder(base, server, "u1", dir) }

	if f, err := check(filepath.Join(home, "new", "Cloud")); err != nil || f.Exists || !f.Empty() {
		t.Fatalf("a new folder: %+v, %v", f, err)
	}
	finder := filepath.Join(home, "finder")
	write(t, filepath.Join(finder, ".DS_Store"), "x")
	if f, err := check(finder); err != nil || !f.Exists || !f.Empty() {
		t.Fatalf("only .DS_Store: %+v, %v", f, err)
	}
	// Files are adopted: allowed, and counted for the window.
	full := filepath.Join(home, "full")
	write(t, filepath.Join(full, "a.txt"), "a")
	write(t, filepath.Join(full, "Docs", "b.txt"), "b")
	if f, err := check(full); err != nil || f.Files != 2 || f.Folders != 1 || f.Replace {
		t.Fatalf("a folder with files: %+v, %v", f, err)
	}
	if _, err := check(filepath.Join(full, "a.txt")); !errors.Is(err, ErrNotFolder) {
		t.Fatalf("a file: %v", err)
	}
	if _, err := check(base); !errors.Is(err, state.ErrNested) {
		t.Fatalf("the state directory: %v", err)
	}
	if _, err := check(filepath.Dir(base)); !errors.Is(err, state.ErrNested) {
		t.Fatalf("around the state directory: %v", err)
	}

	other := setup(t, base, "u2", filepath.Join(home, "Other"))
	for _, dir := range []string{other.LocalRoot, filepath.Join(other.LocalRoot, "in"), home} {
		if _, err := check(dir); !errors.Is(err, state.ErrNested) {
			t.Fatalf("nested with another connection, %s: %v", dir, err)
		}
	}

	// Markers: another connection's (moved out of its place, say) is
	// refused; a gone connection's of the same account is taken over;
	// one of another account or server is refused.
	marked := filepath.Join(home, "marked")
	write(t, filepath.Join(marked, "f.txt"), "f")
	if err := localfs.WriteMarker(marked, localfs.Marker{ConnID: other.ID, Server: server, UserID: "u2"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := check(marked); !errors.Is(err, ErrFolderTaken) {
		t.Fatalf("a registered connection's marker: %v", err)
	}
	for _, tc := range []struct {
		m       localfs.Marker
		replace bool
	}{
		{localfs.Marker{ConnID: "gone1", Server: server, UserID: "u1"}, true},
		{localfs.Marker{ConnID: "gone2"}, true}, // an older client's marker
		{localfs.Marker{ConnID: "gone4", Server: "https://other.example", UserID: "u1"}, false},
		{localfs.Marker{ConnID: "gone3", Server: server, UserID: "u3"}, false},
	} {
		if err := localfs.WriteMarker(marked, tc.m, true); err != nil {
			t.Fatal(err)
		}
		f, err := check(marked)
		if tc.replace && (err != nil || !f.Replace) || !tc.replace && !errors.Is(err, ErrFolderTaken) {
			t.Fatalf("marker %+v: %+v, %v", tc.m, f, err)
		}
	}
	// Before the sign-in only the server is known (the last marker is
	// u3's): checked again by Setup.
	if f, err := CheckFolder(base, server, "", marked); err != nil || !f.Replace {
		t.Fatalf("before the sign-in, the same server: %+v, %v", f, err)
	}
}

// Connecting a folder with files (a reinstall): the top-level names go
// to state.db for the engine to adopt, a gone connection's marker is
// replaced with the new one's.
func TestSetupAdopts(t *testing.T) {
	home, base := testBase(t)
	dir := filepath.Join(home, "Cloud", "old")
	write(t, filepath.Join(dir, "Docs", "a.txt"), "a")
	write(t, filepath.Join(dir, "top.txt"), "t")
	write(t, filepath.Join(dir, ".DS_Store"), "x")
	if err := localfs.WriteMarker(dir, localfs.Marker{ConnID: "gone", Server: server, UserID: "u1"}, false); err != nil {
		t.Fatal(err)
	}
	conn := setup(t, base, "u1", dir)
	m, err := localfs.ReadMarker(dir)
	if err != nil || m != (localfs.Marker{ConnID: conn.ID, Server: server, UserID: "u1"}) {
		t.Fatalf("marker %+v, %v", m, err)
	}
	path, _ := state.Path(conn.ID)
	db, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.View(context.Background(), func(tx *state.Tx) error {
		names, err := tx.Adopted()
		if err != nil || !slices.Equal(names, []string{"Docs", "top.txt"}) {
			t.Errorf("adopted %v, %v", names, err)
		}
		return nil
	})

	// Another account can't have it.
	if _, _, err := Setup(context.Background(), base, signin.Account{Server: server, UserID: "u2", DeviceID: "d"}, filepath.Join(home, "elsewhere")); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(home, "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Setup(context.Background(), base, signin.Account{Server: server, UserID: "u3", DeviceID: "d"}, moved); !errors.Is(err, ErrFolderTaken) {
		t.Fatalf("another account takes a connection's folder: %v", err)
	}
}
