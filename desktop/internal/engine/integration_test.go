package engine

// Against the real server (internal/testserver, a throwaway database on
// the compose postgres; skipped without it): two clients of one user
// (two devices, two folders) and a third party through the API.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/state"
	"github.com/do-service/cloud/desktop/internal/testserver"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		os.Exit(helperMain())
	}
	testserver.Main(m)
}

var fastRetry = api.RetryPolicy{Attempts: 4, MinDelay: 10 * time.Millisecond, MaxDelay: 100 * time.Millisecond}

// counter counts requests by method and path prefix, and can cut the
// network.
type counter struct {
	next    http.RoundTripper
	offline atomic.Bool
	gets    atomic.Int64 // GET …/content: downloads
	sent    atomic.Int64 // bytes of tus PATCH bodies
	creates atomic.Int64 // tus POSTs: uploads begun
}

func (c *counter) RoundTrip(r *http.Request) (*http.Response, error) {
	if c.offline.Load() {
		return nil, errors.New("offline: connection refused")
	}
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/content") {
		c.gets.Add(1)
	}
	if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/uploads/") && r.ContentLength > 0 {
		c.sent.Add(r.ContentLength)
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/v1/uploads/" {
		c.creates.Add(1)
	}
	return c.next.RoundTrip(r)
}

type client struct {
	*harness
	api *api.Client
	net *counter
}

type cloud struct {
	t    *testing.T
	srv  *testserver.Server
	user testserver.User
}

func newCloud(t *testing.T, quota int64) *cloud {
	srv := testserver.Start(t)
	return &cloud{t: t, srv: srv, user: srv.CreateUser(t, "sync", quota)}
}

func (c *cloud) apiClient(opts api.Options) (*api.Client, *counter) {
	c.t.Helper()
	n := &counter{next: &http.Transport{Proxy: nil}}
	opts.Token = api.StaticToken(c.srv.DeviceToken(c.t, c.user))
	opts.HTTPClient = &http.Client{Transport: n}
	if opts.Retry.Attempts == 0 {
		opts.Retry = fastRetry
	}
	a, err := api.New(c.srv.URL, opts)
	if err != nil {
		c.t.Fatal(err)
	}
	return a, n
}

// client is a device of the user with its own folder, everything
// selected.
func (c *cloud) client(device string, tune func(*Config)) *client {
	c.t.Helper()
	a, n := c.apiClient(api.Options{})
	h := newHarness(c.t, a, c.user.ID, func(cfg *Config) {
		cfg.Options.DeviceName = device
		cfg.LockBackoff = 50 * time.Millisecond
		if tune != nil {
			tune(cfg)
		}
	})
	c.t.Cleanup(h.close)
	h.selectAll()
	return &client{harness: h, api: a, net: n}
}

// serverTree is the user's tree as the server lists it.
func (c *cloud) serverTree(a *api.Client) map[string]string {
	c.t.Helper()
	byID := map[string]api.Node{}
	var rootID string
	if _, err := a.Tree(context.Background(), api.RootID, func(n api.Node) error {
		if rootID == "" {
			rootID = n.ID
		}
		byID[n.ID] = n
		return nil
	}); err != nil {
		c.t.Fatal(err)
	}
	var path func(id string) string
	path = func(id string) string {
		n := byID[id]
		if n.ParentID == "" || n.ParentID == rootID {
			return n.Name
		}
		return path(n.ParentID) + "/" + n.Name
	}
	out := map[string]string{}
	for id, n := range byID {
		if id == rootID {
			continue
		}
		if n.Kind == api.KindDir {
			out[path(id)] = "/"
		} else {
			out[path(id)] = n.SHA256
		}
	}
	return out
}

func (c *cloud) expectSynced(a *api.Client, clients ...*client) {
	c.t.Helper()
	want := c.serverTree(a)
	for _, cl := range clients {
		if d := diffTrees(localTree(c.t, cl.root), want); d != "" {
			c.t.Fatalf("%s differs from the server:\n%s", cl.cfg.Options.DeviceName, d)
		}
	}
}

func (c *cloud) nodeAt(a *api.Client, path string) api.Node {
	c.t.Helper()
	var found *api.Node
	byID := map[string]api.Node{}
	if _, err := a.Tree(context.Background(), api.RootID, func(n api.Node) error {
		byID[n.ID] = n
		return nil
	}); err != nil {
		c.t.Fatal(err)
	}
	for _, n := range byID {
		p, cur := n.Name, n
		for cur.ParentID != "" && byID[cur.ParentID].ParentID != "" {
			cur = byID[cur.ParentID]
			p = cur.Name + "/" + p
		}
		if p == path && n.ParentID != "" {
			found = &n
			break
		}
	}
	if found == nil {
		c.t.Fatalf("%s not on the server", path)
	}
	return *found
}

func upload(t *testing.T, a *api.Client, parentID, name, content string) api.UploadResult {
	t.Helper()
	res, err := a.Upload(context.Background(), api.Upload{ParentID: parentID, Name: name, Size: int64(len(content)),
		Body: strings.NewReader(content), OnConflict: api.ConflictFail})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestIntegrationTwoClients(t *testing.T) {
	c := newCloud(t, 0)
	a := c.client("Мак", nil)
	b := c.client("Ноутбук", nil)
	third, _ := c.apiClient(api.Options{})

	// Created on A, appears on B.
	a.write("Документы/отчёт.txt", "v1")
	a.write("Документы/Вложенная/x.bin", "x")
	a.write("корень.txt", "r")
	a.sync()
	b.sync()
	c.expectSynced(third, a, b)

	// Edited, renamed, moved and deleted on B, follows on A.
	b.write("Документы/отчёт.txt", "v2")
	if err := os.Rename(filepath.Join(b.root, "Документы", "Вложенная"), filepath.Join(b.root, "Перенесённая")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(b.root, "корень.txt")); err != nil {
		t.Fatal(err)
	}
	b.sync()
	a.sync()
	c.expectSynced(third, a, b)
	if a.read("Документы/отчёт.txt") != "v2" {
		t.Fatal("edit didn't reach A")
	}
	// The delete went to the server's trash, not away.
	var trash struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := c.srv.JSON(http.MethodGet, "/api/v1/trash", c.srv.Session(t, c.user), nil, &trash); err != nil {
		t.Fatal(err)
	}
	if len(trash.Items) != 1 || trash.Items[0].Name != "корень.txt" {
		t.Fatalf("trash: %+v", trash.Items)
	}

	// A third party: new file, rename, delete.
	docs := c.nodeAt(third, "Документы")
	upload(t, third, docs.ID, "из веба.txt", "web")
	moved := c.nodeAt(third, "Перенесённая/x.bin")
	if _, err := third.Move(context.Background(), moved.ID, docs.ID, "x2.bin", ""); err != nil {
		t.Fatal(err)
	}
	a.sync()
	b.sync()
	c.expectSynced(third, a, b)

	// Both edit one file: the server's version keeps the name, the other
	// becomes a conflict copy that reaches everyone.
	a.write("Документы/отчёт.txt", "edit A")
	b.write("Документы/отчёт.txt", "edit B")
	a.sync()
	b.sync()
	a.sync()
	c.expectSynced(third, a, b)
	tree := c.serverTree(third)
	var copyName string
	for p := range tree {
		if strings.Contains(p, "(конфликт, Ноутбук, ") {
			copyName = p
		}
	}
	if copyName == "" || a.read(copyName) != "edit B" || a.read("Документы/отчёт.txt") != "edit A" {
		t.Fatalf("conflict copy %q in %v", copyName, tree)
	}
	if ps := b.problems(); len(ps) != 1 || ps[0].Kind != ProblemConflict {
		t.Fatalf("B's problems: %+v", ps)
	}
	// Converged: nothing more to do on either.
	for _, cl := range []*client{a, b} {
		if res, err := cl.e.Pass(context.Background()); err != nil || res.Ops != 0 {
			t.Fatalf("extra pass: %+v, %v", res, err)
		}
	}
}

func TestIntegrationFolderMove(t *testing.T) {
	n := 1000
	if os.Getenv("ENGINE_BIG") != "" {
		n = 10000
	}
	c := newCloud(t, 0)
	a := c.client("Мак", nil)
	b := c.client("Ноутбук", nil)
	third, _ := c.apiClient(api.Options{})
	start := time.Now()
	for i := range n {
		a.write(fmt.Sprintf("Проект/данные/%03d/файл-%05d.txt", i%50, i), fmt.Sprint("содержимое ", i))
	}
	a.sync()
	t.Logf("uploaded %d files in %s", n, time.Since(start).Round(time.Millisecond))
	start = time.Now()
	b.sync()
	t.Logf("downloaded them in %s", time.Since(start).Round(time.Millisecond))
	c.expectSynced(third, a, b)

	// One rename of the folder: one PATCH, then one local rename on B,
	// no transfers.
	if err := os.MkdirAll(filepath.Join(a.root, "Архив"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(a.root, "Проект"), filepath.Join(a.root, "Архив", "Проект 2026")); err != nil {
		t.Fatal(err)
	}
	gets := b.net.gets.Load()
	start = time.Now()
	resA := a.sync()
	resB := b.sync()
	t.Logf("moved in %s: A %d ops, B %d ops", time.Since(start).Round(time.Millisecond), resA.Ops, resB.Ops)
	c.expectSynced(third, a, b)
	if resA.Ops > 3 || resB.Ops > 3 {
		t.Errorf("a folder move took %d ops on A and %d on B", resA.Ops, resB.Ops)
	}
	if got := b.net.gets.Load() - gets; got != 0 {
		t.Errorf("%d downloads after a move", got)
	}
}

func TestIntegrationOffline(t *testing.T) {
	c := newCloud(t, 0)
	a := c.client("Мак", nil)
	b := c.client("Ноутбук", nil)
	third, _ := c.apiClient(api.Options{})
	a.write("общий.txt", "1")
	a.write("папка/a.txt", "a")
	a.sync()
	b.sync()

	// A goes offline: its passes fail, nothing is lost or half-done.
	a.net.offline.Store(true)
	a.write("офлайн.txt", "написан без сети")
	a.write("папка/a.txt", "a2")
	if _, err := a.e.SyncOnce(context.Background()); err == nil {
		t.Fatal("sync offline succeeded")
	}
	b.write("с ноутбука.txt", "b")
	if err := os.Remove(filepath.Join(b.root, "общий.txt")); err != nil {
		t.Fatal(err)
	}
	b.sync()
	upload(t, third, c.nodeAt(third, "папка").ID, "веб.txt", "w")

	a.net.offline.Store(false)
	a.sync()
	b.sync()
	c.expectSynced(third, a, b)
	if a.read("папка/a.txt") != "a2" || b.read("офлайн.txt") != "написан без сети" {
		t.Fatal("offline edits lost")
	}
}

func TestIntegrationCursorExpired(t *testing.T) {
	c := newCloud(t, 0)
	a := c.client("Мак", nil)
	third, _ := c.apiClient(api.Options{})
	for i := range 30 {
		a.write(fmt.Sprintf("d/%02d.txt", i), fmt.Sprint(i))
	}
	a.sync()
	// While A doesn't look, one file changes and the journal is cut.
	upload(t, third, c.nodeAt(third, "d").ID, "новый.txt", "new")
	c.srv.ExpireJournal(t, c.user.ID)
	if _, err := a.api.Changes(context.Background(), cursorOf(t, a), 0, 0); !errors.Is(err, api.ErrCursorExpired) {
		t.Fatalf("changes after expiry: %v", err)
	}
	gets := a.net.gets.Load()
	a.sync()
	c.expectSynced(third, a)
	if got := a.net.gets.Load() - gets; got != 1 {
		t.Fatalf("%d downloads after a resync, want 1", got)
	}
}

func cursorOf(t *testing.T, h *client) string {
	var cur string
	if err := h.db.View(context.Background(), func(tx *state.Tx) error {
		a, err := tx.Account()
		cur = a.Cursor
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return cur
}

func TestIntegrationDeviceRevoked(t *testing.T) {
	c := newCloud(t, 0)
	a := c.client("Мак", nil)
	a.write("a.txt", "a")
	a.sync()
	cookie := c.srv.Session(t, c.user)
	var devices []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := c.srv.JSON(http.MethodGet, "/api/v1/me/devices", cookie, nil, &devices); err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		if err := c.srv.JSON(http.MethodDelete, "/api/v1/me/devices/"+d.ID, cookie, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	a.write("b.txt", "b")
	if _, err := a.e.SyncOnce(context.Background()); !errors.Is(err, ErrNeedsSignIn) {
		t.Fatalf("sync after revoke: %v", err)
	}
	ps := a.problems()
	if len(ps) != 1 || ps[0].Kind != ProblemNeedsSignIn {
		t.Fatalf("problems: %+v", ps)
	}
}

func TestIntegrationQuota(t *testing.T) {
	c := newCloud(t, 1<<20)
	a := c.client("Мак", nil)
	a.write("small.txt", "s")
	a.write("big.bin", strings.Repeat("x", 2<<20))
	a.sync()
	third, _ := c.apiClient(api.Options{})
	if tree := c.serverTree(third); tree["small.txt"] == "" || tree["big.bin"] != "" {
		t.Fatalf("server: %v", tree)
	}
	var quota []state.Problem
	for _, p := range a.problems() {
		if p.Kind == ProblemQuota {
			quota = append(quota, p)
		}
	}
	if len(quota) != 1 || quota[0].RelPath != "big.bin" {
		t.Fatalf("problems: %+v", a.problems())
	}
	// Not retried in a loop: the next syncs leave it for its backoff.
	creates := a.net.creates.Load()
	a.sync()
	a.sync()
	if got := a.net.creates.Load() - creates; got != 0 {
		t.Fatalf("%d more upload attempts after a 507", got)
	}
}

// A file locked by a WebDAV client: the upload waits (423, retried) and
// goes through once unlocked; locked for longer than LockRetryFor, it is
// a problem.
func TestIntegrationLocked(t *testing.T) {
	c := newCloud(t, 0)
	a := c.client("Мак", func(cfg *Config) { cfg.LockRetryFor = 300 * time.Millisecond })
	a.write("док.txt", "v1")
	a.sync()

	cookie := c.srv.Session(t, c.user)
	var ap struct {
		Password string `json:"password"`
	}
	if err := c.srv.JSON(http.MethodPost, "/api/v1/me/app-passwords", cookie, map[string]string{"name": "webdav"}, &ap); err != nil {
		t.Fatal(err)
	}
	dav := func(method, token string, body string) *http.Response {
		req, _ := http.NewRequest(method, c.srv.URL+"/dav/files/"+c.user.Username+"/%D0%B4%D0%BE%D0%BA.txt", strings.NewReader(body))
		req.SetBasicAuth(c.user.Username, ap.Password)
		if token != "" {
			req.Header.Set("Lock-Token", token)
		}
		res, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res
	}
	res := dav("LOCK", "", `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("LOCK: %s", res.Status)
	}
	token := res.Header.Get("Lock-Token")

	a.write("док.txt", "v2")
	a.sync()
	if pending := pendingOps(t, a); pending != 1 {
		t.Fatalf("%d ops pending while locked", pending)
	}
	time.Sleep(400 * time.Millisecond)
	a.sync()
	locked := false
	for _, p := range a.problems() {
		locked = locked || p.Kind == ProblemLocked
	}
	if !locked {
		t.Fatalf("no lock problem after LockRetryFor: %+v", a.problems())
	}

	if res := dav("UNLOCK", token, ""); res.StatusCode != http.StatusNoContent {
		t.Fatalf("UNLOCK: %s", res.Status)
	}
	// The next try is due after the long backoff; make it now.
	a.e.mu.Lock()
	a.e.backoff = map[string]*backoff{}
	a.e.mu.Unlock()
	_ = a.db.Update(context.Background(), func(tx *state.Tx) error {
		ops, _ := tx.Ops()
		for _, o := range ops {
			_ = tx.RetryOp(o.ID)
		}
		return nil
	})
	a.sync()
	third, _ := c.apiClient(api.Options{})
	c.expectSynced(third, a)
	if pendingOps(t, a) != 0 {
		t.Fatal("op left after the unlock")
	}
}

func pendingOps(t *testing.T, h *client) int {
	var n int
	_ = h.db.View(context.Background(), func(tx *state.Tx) error {
		ops, err := tx.Ops()
		n = len(ops)
		return err
	})
	return n
}

var _ = bytes.NewReader
