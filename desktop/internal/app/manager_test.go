package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/state"
	"github.com/do-service/cloud/desktop/internal/testserver"
)

// The OS keyring is never touched: go-keyring's mock.
func TestMain(m *testing.M) {
	keyring.MockInit()
	testserver.Main(m)
}

// browser plays the user's browser: signed in to the cloud as u, it
// clicks «Разрешить» on the consent page and follows the redirect back
// to the app.
func browser(t *testing.T, s *testserver.Server, u testserver.User) func(string) error {
	cookie := s.Session(t, u)
	return func(page string) error {
		pu, err := url.Parse(page)
		if err != nil {
			return err
		}
		q := pu.Query()
		go func() {
			redirect, err := s.Authorize(cookie, q.Get("redirect_uri"), q.Get("code_challenge"), q.Get("state"), q.Get("name"))
			if err != nil {
				t.Errorf("authorize: %v", err)
				return
			}
			res, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Get(redirect)
			if err != nil {
				t.Errorf("redirect: %v", err)
				return
			}
			res.Body.Close()
		}()
		return nil
	}
}

func call(t *testing.T, h http.Handler, method, path string, in, out any) int {
	t.Helper()
	var body bytes.Buffer
	if in != nil {
		_ = json.NewEncoder(&body).Encode(in)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, "wails://localhost"+path, &body))
	if out != nil && rec.Code < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, rec.Body)
		}
	}
	return rec.Code
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestAddBrowseSelectRemove goes through the window's API as the window
// does: add a cloud, browse it through the proxy with the device token,
// check a folder, see it synced, remove the connection.
func TestAddBrowseSelectRemove(t *testing.T) {
	s := testserver.Start(t)
	u := s.CreateUser(t, "app", 0)
	var docs struct {
		ID string `json:"id"`
	}
	if err := s.JSON(http.MethodPost, "/api/v1/nodes/root/children", s.Session(t, u), map[string]string{"name": "Документы"}, &docs); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "state"))
	base, err := state.BaseDir()
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Options{Base: base, Store: signin.NewStore(base), OpenBrowser: browser(t, s, u)})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	mux := http.NewServeMux()
	mux.Handle("/app/", m.Handler())
	mux.Handle("/api/", NewProxy(m.Upstream))

	if code := call(t, mux, http.MethodPost, "/app/v1/signin", map[string]string{"server": "ftp://nope"}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad address: %d", code)
	}
	var st SignInState
	if code := call(t, mux, http.MethodPost, "/app/v1/signin", map[string]string{"server": s.URL}, &st); code != http.StatusAccepted || st.State != "waiting" {
		t.Fatalf("start sign-in: %d %+v", code, st)
	}
	waitFor(t, "the sign-in", func() bool {
		call(t, mux, http.MethodGet, "/app/v1/signin", nil, &st)
		return st.State != "waiting"
	})
	if st.State != "done" || st.ConnID == "" {
		t.Fatalf("sign-in: %+v", st)
	}
	conn := st.ConnID
	var list struct {
		Items  []ConnInfo `json:"items"`
		Active string     `json:"active"`
	}
	waitFor(t, "the first listing", func() bool {
		call(t, mux, http.MethodGet, "/app/v1/connections", nil, &list)
		return len(list.Items) == 1 && list.Items[0].Status == StatusSynced
	})
	c := list.Items[0]
	if list.Active != conn || c.User != u.Username || c.Folder != filepath.Join(home, "Cloud", strings.ReplaceAll(strings.TrimPrefix(s.URL, "http://"), ":", "-")) {
		t.Fatalf("connection %+v, active %s", c, list.Active)
	}
	if err := localfs.CheckMarker(c.Folder, conn); err != nil {
		t.Fatal(err)
	}

	// The window's /api reaches the server with the device token.
	var page struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if code := call(t, mux, http.MethodGet, "/api/v1/nodes/root/children", nil, &page); code != http.StatusOK || len(page.Items) != 1 || page.Items[0].ID != docs.ID {
		t.Fatalf("listing through the proxy: %d %+v", code, page)
	}

	// The CLI, or a second app, can't open the connection meanwhile.
	if _, err := state.LockConnection(conn); !errors.Is(err, state.ErrLocked) {
		t.Fatalf("lock while the app runs: %v", err)
	}

	var marks struct {
		Items map[string]struct{ Check, Sync string } `json:"items"`
	}
	ids := map[string][]string{"ids": {docs.ID}}
	if call(t, mux, http.MethodPost, "/app/v1/connections/"+conn+"/marks", ids, &marks); marks.Items[docs.ID].Check != "off" {
		t.Fatalf("marks before checking: %+v", marks)
	}
	if code := call(t, mux, http.MethodPut, "/app/v1/connections/"+conn+"/selection/"+docs.ID, nil, nil); code != http.StatusOK {
		t.Fatalf("check: %d", code)
	}
	waitFor(t, "the checked folder on disk", func() bool {
		_, err := os.Stat(filepath.Join(c.Folder, "Документы"))
		return err == nil
	})
	waitFor(t, "the folder synced", func() bool {
		call(t, mux, http.MethodPost, "/app/v1/connections/"+conn+"/marks", ids, &marks)
		return marks.Items[docs.ID] == struct{ Check, Sync string }{"on", "synced"}
	})

	if code := call(t, mux, http.MethodPost, "/app/v1/connections/"+conn+"/pause", nil, nil); code != http.StatusNoContent {
		t.Fatalf("pause: %d", code)
	}
	waitFor(t, "paused", func() bool {
		call(t, mux, http.MethodGet, "/app/v1/connections", nil, &list)
		return list.Items[0].Status == StatusPaused
	})

	if code := call(t, mux, http.MethodDelete, "/app/v1/connections/"+conn, nil, nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	call(t, mux, http.MethodGet, "/app/v1/connections", nil, &list)
	if len(list.Items) != 0 || list.Active != "" {
		t.Fatalf("after removing: %+v", list)
	}
	// The files stay, as a plain folder; the token is revoked.
	if _, err := os.Stat(filepath.Join(c.Folder, "Документы")); err != nil {
		t.Fatalf("the folder went with the connection: %v", err)
	}
	if err := localfs.CheckMarker(c.Folder, conn); !errors.Is(err, localfs.ErrNoMarker) {
		t.Fatalf("marker after removing: %v", err)
	}
	if code := call(t, mux, http.MethodGet, "/api/v1/nodes/root/children", nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("proxy after removing: %d", code)
	}
	if _, err := signin.NewStore(base).Token(s.URL, u.ID); !errors.Is(err, signin.ErrNoToken) {
		t.Fatalf("token after removing: %v", err)
	}
}
