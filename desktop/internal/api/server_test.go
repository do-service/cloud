package api_test

// Against the real server (internal/testserver): the client's half of
// the contract of /sync/*, the nodes API, tus and downloads.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	"github.com/do-service/cloud/desktop/internal/testserver"
)

func TestMain(m *testing.M) { testserver.Main(m) }

var fastRetry = api.RetryPolicy{Attempts: 5, MinDelay: 5 * time.Millisecond, MaxDelay: 50 * time.Millisecond}

type conn struct {
	srv   *testserver.Server
	user  testserver.User
	token string
	c     *api.Client
}

// connect makes a user of its own and signs them in as a device.
func connect(t *testing.T, opts api.Options) conn {
	t.Helper()
	s := testserver.Start(t)
	u := s.CreateUser(t, "sync", 0)
	tok := s.DeviceToken(t, u)
	return conn{srv: s, user: u, token: tok, c: newClient(t, s, tok, opts)}
}

func newClient(t *testing.T, s *testserver.Server, token string, opts api.Options) *api.Client {
	t.Helper()
	opts.Token = api.StaticToken(token)
	if opts.Retry.Attempts == 0 {
		opts.Retry = fastRetry
	}
	c, err := api.New(s.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func upload(t *testing.T, c *api.Client, u api.Upload, content []byte) api.UploadResult {
	t.Helper()
	u.Body, u.Size = bytes.NewReader(content), int64(len(content))
	res, err := c.Upload(context.Background(), u)
	if err != nil {
		t.Fatalf("upload %q: %v", u.Name, err)
	}
	return res
}

func TestInfo(t *testing.T) {
	s := testserver.Start(t)
	c, err := api.New(s.URL+"/", api.Options{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.APIVersion != 1 || info.MaxWait != 55*time.Second || info.JournalRetention <= 0 || info.ServerVersion == "" {
		t.Errorf("info = %+v", info)
	}
}

func TestSyncRoundTrip(t *testing.T) {
	ctx := context.Background()
	cn := connect(t, api.Options{})
	c := cn.c

	roots, err := c.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots.Roots) != 1 || roots.Roots[0].Kind != "own" || roots.Roots[0].Owner.ID != cn.user.ID || roots.Cursor == "" {
		t.Fatalf("roots = %+v", roots)
	}
	rootID := roots.Roots[0].ID

	docs, err := c.Mkdir(ctx, api.RootID, "Документы")
	if err != nil {
		t.Fatal(err)
	}
	if docs.Kind != api.KindDir || docs.ParentID != rootID {
		t.Errorf("mkdir = %+v", docs)
	}
	if _, err := c.Mkdir(ctx, rootID, "Документы"); !errors.Is(err, api.ErrNameConflict) {
		t.Errorf("mkdir twice: %v, want ErrNameConflict", err)
	}
	content := []byte("hello, sync")
	mtime := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	up := upload(t, c, api.Upload{ParentID: docs.ID, Name: "a.txt", OnConflict: api.ConflictFail, MTime: mtime}, content)
	if up.NodeID == "" || up.ETag == "" {
		t.Fatalf("upload = %+v", up)
	}

	ch, err := c.Changes(ctx, roots.Cursor, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]api.Change{}
	for _, x := range ch.Changes {
		byID[x.ID] = x
	}
	if d := byID[docs.ID]; d.Node == nil || d.Node.Name != "Документы" || d.Owner != cn.user.ID {
		t.Errorf("changes: folder = %+v", d)
	}
	f := byID[up.NodeID]
	if f.Node == nil || f.Node.ParentID != docs.ID || f.Node.SHA256 != sum(content) || f.Node.ETag != up.ETag ||
		f.Node.Size != int64(len(content)) || !f.Node.MTime.Equal(mtime) || !f.Node.Can("write") {
		t.Errorf("changes: file = %+v", f.Node)
	}

	// Tree: the root first, a folder before what's in it.
	var seen []api.Node
	cursor, err := c.Tree(ctx, api.RootID, func(n api.Node) error {
		seen = append(seen, n)
		return nil
	})
	if err != nil || cursor == "" {
		t.Fatalf("tree: %q, %v", cursor, err)
	}
	if len(seen) != 3 || seen[0].ID != rootID || seen[0].ParentID != "" || seen[1].ID != docs.ID || seen[2].ID != up.NodeID {
		t.Errorf("tree = %+v", seen)
	}

	// Rename with a stale base is refused; with the current one it goes.
	if _, err := c.Move(ctx, up.NodeID, "", "b.txt", "stale"); !errors.Is(err, api.ErrPrecondition) {
		t.Errorf("move with a stale etag: %v, want ErrPrecondition", err)
	}
	moved, err := c.Move(ctx, up.NodeID, api.RootID, "b.txt", up.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Name != "b.txt" || moved.ParentID != rootID {
		t.Errorf("moved = %+v", moved)
	}
	got, err := c.Get(ctx, up.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "b.txt" || len(got.Path) != 2 || got.Path[1].ID != up.NodeID {
		t.Errorf("get = %+v", got)
	}

	ch2, err := c.Changes(ctx, ch.Cursor, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch2.Changes) != 1 || ch2.Changes[0].Node == nil || ch2.Changes[0].Node.Name != "b.txt" {
		t.Errorf("changes after move = %+v", ch2.Changes)
	}

	if err := c.Trash(ctx, up.NodeID, "stale"); !errors.Is(err, api.ErrPrecondition) {
		t.Errorf("trash with a stale etag: %v, want ErrPrecondition", err)
	}
	if err := c.Trash(ctx, up.NodeID, moved.ETag); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, up.NodeID); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("get trashed: %v, want ErrNotFound", err)
	}
	ch3, err := c.Changes(ctx, ch2.Cursor, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch3.Changes) != 1 || !ch3.Changes[0].Deleted || ch3.Changes[0].ID != up.NodeID {
		t.Errorf("changes after trash = %+v", ch3.Changes)
	}

	// A long poll with nothing new comes back empty with the same place.
	start := time.Now()
	ch4, err := c.Changes(ctx, ch3.Cursor, 10, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch4.Changes) != 0 || ch4.More || time.Since(start) > 30*time.Second {
		t.Errorf("idle changes = %+v", ch4)
	}
}

func TestChangesCursorExpired(t *testing.T) {
	cn := connect(t, api.Options{})
	roots, err := cn.c.Roots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A cursor ahead of the journal (a server restored from a backup) is
	// as unusable as one behind it.
	raw, err := base64.RawURLEncoding.DecodeString(roots.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	var cur map[string]any
	if err := json.Unmarshal(raw, &cur); err != nil {
		t.Fatal(err)
	}
	cur["own"] = 1 << 40
	raw, _ = json.Marshal(cur)
	_, err = cn.c.Changes(context.Background(), base64.RawURLEncoding.EncodeToString(raw), 0, 0)
	if !errors.Is(err, api.ErrCursorExpired) {
		t.Fatalf("err = %v, want ErrCursorExpired", err)
	}
}

func TestUploadConditional(t *testing.T) {
	ctx := context.Background()
	cn := connect(t, api.Options{ChunkSize: 1 << 10})
	c := cn.c

	v1 := upload(t, c, api.Upload{ParentID: api.RootID, Name: "doc.txt"}, []byte("v1"))
	v2 := upload(t, c, api.Upload{NodeID: v1.NodeID, IfMatch: v1.ETag}, []byte("version 2"))
	if v2.NodeID != v1.NodeID || v2.ETag == v1.ETag {
		t.Fatalf("v2 = %+v after %+v", v2, v1)
	}

	// Based on v1, which is no longer there: refused at creation.
	_, err := c.Upload(ctx, api.Upload{NodeID: v1.NodeID, IfMatch: v1.ETag, Size: 2, Body: strings.NewReader("v3")})
	if !errors.Is(err, api.ErrPrecondition) {
		t.Errorf("upload on a stale base: %v, want ErrPrecondition", err)
	}

	// Changed by someone else while uploading: refused at the end.
	other := newClient(t, cn.srv, cn.token, api.Options{})
	content := randomBytes(t, 4<<10)
	changed := false
	_, err = c.Upload(ctx, api.Upload{
		NodeID: v1.NodeID, IfMatch: v2.ETag, Size: int64(len(content)), Body: bytes.NewReader(content),
		Progress: func(url string, offset int64) error {
			if offset > 0 && !changed {
				changed = true
				upload(t, other, api.Upload{NodeID: v1.NodeID}, []byte("someone else's"))
			}
			return nil
		},
	})
	if !changed || !errors.Is(err, api.ErrPrecondition) {
		t.Errorf("upload with a change midway: %v, want ErrPrecondition", err)
	}

	// A taken name with onConflict=fail.
	_, err = c.Upload(ctx, api.Upload{ParentID: api.RootID, Name: "doc.txt", OnConflict: api.ConflictFail, Size: 1, Body: strings.NewReader("x")})
	if !errors.Is(err, api.ErrNameConflict) {
		t.Errorf("upload onto a taken name: %v, want ErrNameConflict", err)
	}

	// An empty file is complete at creation.
	empty := upload(t, c, api.Upload{ParentID: api.RootID, Name: "empty"}, nil)
	n, err := c.Get(ctx, empty.NodeID)
	if err != nil || n.Size != 0 || n.ETag != empty.ETag {
		t.Errorf("empty file = %+v, %v", n, err)
	}
}

func TestUploadQuota(t *testing.T) {
	s := testserver.Start(t)
	u := s.CreateUser(t, "quota", 1000)
	c := newClient(t, s, s.DeviceToken(t, u), api.Options{})
	_, err := c.Upload(context.Background(), api.Upload{ParentID: api.RootID, Name: "big", Size: 2000, Body: bytes.NewReader(make([]byte, 2000))})
	if !errors.Is(err, api.ErrQuota) {
		t.Fatalf("err = %v, want ErrQuota", err)
	}
}

// cutTransport fails a request of method once, after its body has sent
// (or its response delivered) cutAfter bytes, as a dropped connection
// would. It counts the body bytes that went out and came in.
type cutTransport struct {
	method   string
	cutAfter int64
	cut      atomic.Bool
	sent     atomic.Int64
	received atomic.Int64
}

// errCut is what a dropped connection looks like to a body reader.
var errCut = fmt.Errorf("connection cut by the test: %w", io.ErrUnexpectedEOF)

func (ct *cutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	arm := req.Method == ct.method && ct.cut.CompareAndSwap(false, true)
	if req.Body != nil {
		body := &countingReader{r: req.Body, n: &ct.sent}
		if arm && req.Method != http.MethodGet {
			body.limit = ct.cutAfter
		}
		req.Body = io.NopCloser(body)
	}
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	rb := &countingReader{r: res.Body, n: &ct.received}
	if arm && req.Method == http.MethodGet {
		rb.limit = ct.cutAfter
	}
	res.Body = struct {
		io.Reader
		io.Closer
	}{rb, res.Body}
	return res, nil
}

type countingReader struct {
	r     io.Reader
	n     *atomic.Int64
	read  int64
	limit int64 // 0: none
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.limit > 0 {
		if c.read >= c.limit {
			return 0, errCut
		}
		p = p[:min(int64(len(p)), c.limit-c.read)]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	c.n.Add(int64(n))
	return n, err
}

func TestUploadResumeMidChunk(t *testing.T) {
	ctx := context.Background()
	ct := &cutTransport{method: http.MethodPatch, cutAfter: 100 << 10}
	cn := connect(t, api.Options{ChunkSize: 256 << 10, HTTPClient: &http.Client{Transport: ct}})
	content := randomBytes(t, 1<<20)

	var offsets []int64
	res, err := cn.c.Upload(ctx, api.Upload{
		ParentID: api.RootID, Name: "resumed.bin", SHA256: sum(content), Size: int64(len(content)), Body: bytes.NewReader(content),
		Progress: func(url string, offset int64) error {
			offsets = append(offsets, offset)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ct.cut.Load() {
		t.Fatal("the transport never cut a PATCH")
	}
	// The server kept what arrived before the cut: the HEAD after it
	// reports an offset inside the first chunk, not 0.
	if len(offsets) < 2 || offsets[0] != 0 || offsets[1] <= 0 || offsets[1] >= 256<<10 {
		t.Errorf("progress offsets = %v", offsets)
	}
	if sent := ct.sent.Load(); sent > int64(len(content))+(100<<10) {
		t.Errorf("sent %d bytes for %d", sent, len(content))
	}
	checkContent(t, cn.c, res, content)
}

func TestUploadResumeAfterRestart(t *testing.T) {
	ctx := context.Background()
	ct := &cutTransport{}
	cn := connect(t, api.Options{ChunkSize: 256 << 10, HTTPClient: &http.Client{Transport: ct}})
	content := randomBytes(t, 1<<20)

	// The first run stops after two chunks, as a quit app would; what it
	// saved is the URL and offset.
	var savedURL string
	var savedOffset int64
	errQuit := errors.New("quit")
	_, err := cn.c.Upload(ctx, api.Upload{
		ParentID: api.RootID, Name: "restart.bin", Size: int64(len(content)), Body: bytes.NewReader(content),
		Progress: func(url string, offset int64) error {
			savedURL, savedOffset = url, offset
			if offset >= 512<<10 {
				return errQuit
			}
			return nil
		},
	})
	if !errors.Is(err, errQuit) || savedOffset != 512<<10 || !strings.HasPrefix(savedURL, cn.srv.URL+"/api/v1/uploads/") {
		t.Fatalf("first run: %v at %q+%d", err, savedURL, savedOffset)
	}

	ct2 := &cutTransport{}
	c2 := newClient(t, cn.srv, cn.token, api.Options{ChunkSize: 256 << 10, HTTPClient: &http.Client{Transport: ct2}})
	res, err := c2.Upload(ctx, api.Upload{
		ParentID: api.RootID, Name: "restart.bin", Size: int64(len(content)), Body: bytes.NewReader(content), URL: savedURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent := ct2.sent.Load(); sent != int64(len(content))-savedOffset {
		t.Errorf("second run sent %d bytes, want the remaining %d", sent, int64(len(content))-savedOffset)
	}
	checkContent(t, c2, res, content)

	// An upload URL that is gone (finished, here) starts a new upload.
	again, err := c2.Upload(ctx, api.Upload{
		ParentID: api.RootID, Name: "restart.bin", Size: int64(len(content)), Body: bytes.NewReader(content), URL: savedURL,
	})
	if err != nil || again.NodeID == res.NodeID {
		t.Errorf("upload with a finished URL: %+v, %v", again, err)
	}
}

func checkContent(t *testing.T, c *api.Client, res api.UploadResult, content []byte) {
	t.Helper()
	n, err := c.Get(context.Background(), res.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if n.SHA256 != sum(content) || n.Size != int64(len(content)) || n.ETag != res.ETag {
		t.Errorf("uploaded node = %+v, want sha256 %s", n.Node, sum(content))
	}
}

func TestDownloadResume(t *testing.T) {
	ctx := context.Background()
	ct := &cutTransport{method: http.MethodGet, cutAfter: 200 << 10}
	cn := connect(t, api.Options{HTTPClient: &http.Client{Transport: ct}})
	content := randomBytes(t, 1<<20)
	up := upload(t, cn.c, api.Upload{ParentID: api.RootID, Name: "down.bin"}, content)
	dir := t.TempDir()

	// Cut midway (the first GET, armed above): carried on from where
	// it stopped.
	part := filepath.Join(dir, "a.part")
	ct.received.Store(0)
	res, err := cn.c.DownloadFile(ctx, up.NodeID, part, api.DownloadOptions{SHA256: sum(content)})
	if err != nil {
		t.Fatal(err)
	}
	if !ct.cut.Load() || res.ETag != up.ETag || res.SHA256 != sum(content) || res.Size != int64(len(content)) {
		t.Errorf("download = %+v", res)
	}
	if got := ct.received.Load(); got != int64(len(content)) {
		t.Errorf("received %d bytes for %d: a cut download started over", got, len(content))
	}
	checkFile(t, part, content)

	// A restart with a third of it on disk asks for the rest only.
	part2 := filepath.Join(dir, "b.part")
	if err := os.WriteFile(part2, content[:300<<10], 0o644); err != nil {
		t.Fatal(err)
	}
	ct.received.Store(0)
	res, err = cn.c.DownloadFile(ctx, up.NodeID, part2, api.DownloadOptions{ETag: up.ETag, SHA256: sum(content)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resumed || ct.received.Load() != int64(len(content)-300<<10) {
		t.Errorf("resumed download: %+v, received %d", res, ct.received.Load())
	}
	checkFile(t, part2, content)

	// Complete already: nothing to fetch.
	res, err = cn.c.DownloadFile(ctx, up.NodeID, part2, api.DownloadOptions{ETag: up.ETag})
	if err != nil || res.SHA256 != sum(content) {
		t.Errorf("download of a complete part: %+v, %v", res, err)
	}

	// The content changed since the part was written: the old bytes go.
	v2 := randomBytes(t, 100<<10)
	up2 := upload(t, cn.c, api.Upload{NodeID: up.NodeID, IfMatch: up.ETag}, v2)
	part3 := filepath.Join(dir, "c.part")
	if err := os.WriteFile(part3, content[:300<<10], 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = cn.c.DownloadFile(ctx, up.NodeID, part3, api.DownloadOptions{ETag: up.ETag, SHA256: sum(content)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resumed || res.ETag != up2.ETag || res.SHA256 != sum(v2) {
		t.Errorf("download of changed content = %+v", res)
	}
	checkFile(t, part3, v2)

	// A wrong expected hash is refused, and the part emptied.
	part4 := filepath.Join(dir, "d.part")
	if _, err := cn.c.DownloadFile(ctx, up.NodeID, part4, api.DownloadOptions{SHA256: sum(content)}); !errors.Is(err, api.ErrChecksum) {
		t.Errorf("download with a wrong sha256: %v, want ErrChecksum", err)
	}
	if fi, err := os.Stat(part4); err != nil || fi.Size() != 0 {
		t.Errorf("part after a checksum mismatch: %v, %v", fi, err)
	}

	if _, err := cn.c.DownloadFile(ctx, "00000000-0000-0000-0000-000000000000", filepath.Join(dir, "e.part"), api.DownloadOptions{}); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("download of a missing node: %v, want ErrNotFound", err)
	}
}

func checkFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s: %d bytes, sha256 %s; want %d, %s", filepath.Base(path), len(got), sum(got), len(want), sum(want))
	}
}

func TestUnauthorized(t *testing.T) {
	ctx := context.Background()
	cn := connect(t, api.Options{})
	if _, err := cn.c.Roots(ctx); err != nil {
		t.Fatal(err)
	}
	bad := newClient(t, cn.srv, "cld_not-a-token", api.Options{})
	if _, err := bad.Roots(ctx); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("unknown token: %v, want ErrUnauthorized", err)
	}

	if err := cn.c.SignOut(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := cn.c.Roots(ctx); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("after sign-out: %v, want ErrUnauthorized", err)
	}
	if _, err := cn.c.Upload(ctx, api.Upload{ParentID: api.RootID, Name: "x", Size: 1, Body: strings.NewReader("x")}); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("upload after sign-out: %v, want ErrUnauthorized", err)
	}
	if err := cn.c.SignOut(ctx); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("second sign-out: %v, want ErrUnauthorized", err)
	}
}

func TestExchangeCodeRefused(t *testing.T) {
	s := testserver.Start(t)
	c, err := api.New(s.URL, api.Options{Retry: fastRetry})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ExchangeCode(context.Background(), "no-such-code", strings.Repeat("v", 43), "http://127.0.0.1:50000/cb")
	if errors.Is(err, api.ErrNotFound) {
		t.Skip("E12-2 isn't on the server yet")
	}
	if !errors.Is(err, api.ErrInvalidGrant) {
		t.Errorf("unknown code: %v, want ErrInvalidGrant", err)
	}
}
