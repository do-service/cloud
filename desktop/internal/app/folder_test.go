package app

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/do-service/cloud/desktop/internal/connection"
	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/state"
	"github.com/do-service/cloud/desktop/internal/testserver"
)

// TestChangeFolder: add a cloud into a chosen folder, sync a folder,
// then move the sync folder while the engine runs; the engine stops,
// the folder moves, and sync goes on in the new place.
func TestChangeFolder(t *testing.T) {
	s := testserver.Start(t)
	u := s.CreateUser(t, "mv", 0)
	cookie := s.Session(t, u)
	var docs struct {
		ID string `json:"id"`
	}
	if err := s.JSON(http.MethodPost, "/api/v1/nodes/root/children", cookie, map[string]string{"name": "Документы"}, &docs); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	base, err := state.BaseDir()
	if err != nil {
		t.Fatal(err)
	}
	picked := filepath.Join(home, "Picked")
	m, err := NewManager(Options{Base: base, Store: signin.NewStore(base), OpenBrowser: browser(t, s, u),
		PickFolder: func(string) (string, error) { return picked, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	h := m.Handler()

	// «Добавить облако»: the default folder, then one chosen; a file in
	// the way is refused before the browser opens.
	var f struct {
		connection.Folder
		Code string `json:"code"`
	}
	q := "/app/v1/new-folder?server=" + url.QueryEscape(s.URL)
	if code := call(t, h, http.MethodGet, q, nil, &f); code != http.StatusOK || f.Path != filepath.Join(home, "Cloud", filepath.Base(f.Path)) || f.Exists || f.Code != "" {
		t.Fatalf("default folder: %d %+v", code, f)
	}
	if err := os.WriteFile(filepath.Join(home, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if call(t, h, http.MethodGet, q+"&path="+url.QueryEscape(filepath.Join(home, "file")), nil, &f); f.Code != "not_a_folder" {
		t.Fatalf("a file: %+v", f)
	}
	if code := call(t, h, http.MethodPost, "/app/v1/signin", map[string]string{"server": s.URL, "folder": filepath.Join(home, "file")}, nil); code != http.StatusConflict {
		t.Fatalf("sign-in into a file: %d", code)
	}
	var pick struct{ Path string }
	if call(t, h, http.MethodPost, "/app/v1/pick-folder", map[string]string{"start": home}, &pick); pick.Path != picked {
		t.Fatalf("pick: %+v", pick)
	}
	first := filepath.Join(home, "First")
	var st SignInState
	if code := call(t, h, http.MethodPost, "/app/v1/signin", map[string]string{"server": s.URL, "folder": first}, &st); code != http.StatusAccepted {
		t.Fatalf("sign-in: %d", code)
	}
	waitFor(t, "the sign-in", func() bool {
		call(t, h, http.MethodGet, "/app/v1/signin", nil, &st)
		return st.State != "waiting"
	})
	if st.State != "done" {
		t.Fatalf("sign-in: %+v", st)
	}
	id := st.ConnID
	var list struct {
		Items []ConnInfo `json:"items"`
	}
	status := func() ConnInfo {
		call(t, h, http.MethodGet, "/app/v1/connections", nil, &list)
		return list.Items[0]
	}
	waitFor(t, "synced", func() bool { return status().Status == StatusSynced })
	if c := status(); c.Folder != first {
		t.Fatalf("folder %s, want %s", c.Folder, first)
	}
	if code := call(t, h, http.MethodPut, "/app/v1/connections/"+id+"/selection/"+docs.ID, nil, nil); code != http.StatusOK {
		t.Fatalf("check: %d", code)
	}
	waitFor(t, "Документы on disk", func() bool {
		_, err := os.Stat(filepath.Join(first, "Документы"))
		return err == nil
	})

	// Refused without stopping sync: not empty, inside the folder.
	full := filepath.Join(home, "Full")
	if err := os.MkdirAll(filepath.Join(full, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{full: "folder_not_empty", filepath.Join(first, "Документы"): "folder_inside"} {
		if call(t, h, http.MethodGet, "/app/v1/connections/"+id+"/folder?path="+url.QueryEscape(path), nil, &f); f.Code != want {
			t.Fatalf("check %s: %+v, want %s", path, f, want)
		}
		rec := call(t, h, http.MethodPut, "/app/v1/connections/"+id+"/folder", map[string]string{"path": path}, nil)
		if rec != http.StatusConflict {
			t.Fatalf("move to %s: %d", path, rec)
		}
	}
	if c := status(); c.Folder != first || c.Status != StatusSynced {
		t.Fatalf("after refusals: %+v", c)
	}

	// The move, with the engine running.
	if err := os.MkdirAll(picked, 0o755); err != nil {
		t.Fatal(err)
	}
	var moved struct{ Folder string }
	if code := call(t, h, http.MethodPut, "/app/v1/connections/"+id+"/folder", map[string]string{"path": picked}, &moved); code != http.StatusOK || moved.Folder != picked {
		t.Fatalf("move: %d %+v", code, moved)
	}
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old folder: %v", err)
	}
	if err := localfs.CheckMarker(picked, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(picked, "Документы")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "synced in the new place", func() bool { c := status(); return c.Status == StatusSynced && c.Folder == picked })
	// The engine runs there: the server's change comes to the new place.
	if err := s.JSON(http.MethodPost, "/api/v1/nodes/"+docs.ID+"/children", cookie, map[string]string{"name": "Отчёты"}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Отчёты in the new place", func() bool {
		_, err := os.Stat(filepath.Join(picked, "Документы", "Отчёты"))
		return err == nil
	})
	if got, err := state.OpenRegistry(base).Get(id); err != nil || got.LocalRoot != picked || got.MovingTo != "" {
		t.Fatalf("registry %+v, %v", got, err)
	}
}
