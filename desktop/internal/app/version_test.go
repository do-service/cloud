package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/state"
	"github.com/do-service/cloud/desktop/internal/testserver"
)

// TestClientTooOld (E15-4): the server raises minClientVersion above this
// client's while it syncs. The connection stops with too_old and the
// version it needs — not an error retried in a loop — and goes back to
// syncing once the server takes the client again.
func TestClientTooOld(t *testing.T) {
	s := testserver.Start(t)
	u := s.CreateUser(t, "old", 0)

	// The server as the app sees it: /sync/info's minClientVersion is
	// whatever min holds ("" passes the server's through).
	var min atomic.Value
	min.Store("")
	var infos atomic.Int64
	target, _ := url.Parse(s.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(res *http.Response) error {
		v, _ := min.Load().(string)
		if res.Request.URL.Path != "/api/v1/sync/info" {
			return nil
		}
		infos.Add(1)
		if v == "" {
			return nil
		}
		var info map[string]any
		if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
			return err
		}
		res.Body.Close()
		info["minClientVersion"] = v
		data, _ := json.Marshal(info)
		res.Body = io.NopCloser(bytes.NewReader(data))
		res.ContentLength = int64(len(data))
		res.Header.Set("Content-Length", strconv.Itoa(len(data)))
		return nil
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "state"))
	base, err := state.BaseDir()
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Options{Base: base, Store: signin.NewStore(base), OpenBrowser: browser(t, s, u), VersionCheck: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	h := m.Handler()

	var st SignInState
	if code := call(t, h, http.MethodPost, "/app/v1/signin", map[string]string{"server": front.URL}, &st); code != http.StatusAccepted {
		t.Fatalf("sign-in: %d", code)
	}
	waitFor(t, "the sign-in", func() bool {
		call(t, h, http.MethodGet, "/app/v1/signin", nil, &st)
		return st.State != "waiting"
	})
	if st.State != "done" {
		t.Fatalf("sign-in: %+v", st)
	}
	var list struct {
		Items []ConnInfo `json:"items"`
	}
	status := func() ConnInfo {
		call(t, h, http.MethodGet, "/app/v1/connections", nil, &list)
		if len(list.Items) != 1 {
			t.Fatalf("connections: %+v", list.Items)
		}
		return list.Items[0]
	}
	waitFor(t, "synced", func() bool { return status().Status == StatusSynced })

	min.Store("99.0.0")
	waitFor(t, "turned away", func() bool { return status().Status == StatusTooOld })
	if c := status(); c.MinVersion != "99.0.0" {
		t.Fatalf("too old: %+v", c)
	}
	if Overall(list.Items) != StatusError {
		t.Fatalf("the tray: %s", Overall(list.Items))
	}
	// It stays so, asking only every VersionCheck.
	before := infos.Load()
	time.Sleep(time.Second)
	if c := status(); c.Status != StatusTooOld {
		t.Fatalf("after a while: %+v", c)
	}
	if n := infos.Load() - before; n > 10 {
		t.Fatalf("asked /sync/info %d times in a second", n)
	}

	// The server takes the client again (or the app was updated).
	min.Store("")
	waitFor(t, "syncing again", func() bool { return status().Status == StatusSynced })
}
