package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/db"
	"github.com/do-service/cloud/backend/internal/storage"
	"github.com/do-service/cloud/backend/internal/storage/localfs"
)

type env struct {
	svc     *Service
	pool    *pgxpool.Pool
	dataDir string
	user    string
	root    Node
}

func testDatabaseURL() string {
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
}

// newEnv gives each test an empty database and data dir, a user and their
// root. wrap, if set, wraps the storage backend (to inject failures).
func newEnv(t *testing.T, wrap func(storage.Backend) storage.Backend) env {
	t.Helper()
	url := testDatabaseURL()
	if err := db.Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, url, 20, 0, 10*time.Second)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE users, nodes, blobs, versions CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	dir := t.TempDir()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	var backend storage.Backend = store
	if wrap != nil {
		backend = wrap(store)
	}

	e := env{svc: New(pool, backend), pool: pool, dataDir: dir}
	e.user = e.newUser(t, "alice")
	if e.root, err = e.svc.EnsureRoot(ctx, e.user); err != nil {
		t.Fatalf("root: %v", err)
	}
	return e
}

func (e env) newUser(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO users (username) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func (e env) mkdir(t *testing.T, parent, name string) Node {
	t.Helper()
	n, err := e.svc.CreateDir(context.Background(), e.user, parent, name)
	if err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	return n
}

func (e env) file(t *testing.T, parent, name, content string) Node {
	t.Helper()
	n, err := e.svc.CreateFile(context.Background(), e.user, parent, name, strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("create file %s: %v", name, err)
	}
	return n
}

func (e env) get(t *testing.T, id string) Node {
	t.Helper()
	n, err := e.svc.Get(context.Background(), e.user, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return n
}

// onDisk returns the content at the user's relative path, or "" with
// ok=false when it doesn't exist.
func (e env) onDisk(t *testing.T, rel string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.dataDir, "files", e.user, filepath.FromSlash(rel)))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data), true
}

// checkSizes asserts the invariant every write must keep: a folder's size
// is the sum of its live children's sizes.
func (e env) checkSizes(t *testing.T) {
	t.Helper()
	var bad int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM nodes d
		WHERE d.kind = 'dir' AND d.trashed_at IS NULL
		  AND d.size <> coalesce((SELECT sum(c.size) FROM nodes c WHERE c.parent_id = d.id AND c.trashed_at IS NULL), 0)`).Scan(&bad); err != nil {
		t.Fatalf("check sizes: %v", err)
	}
	if bad != 0 {
		t.Fatalf("%d folders have a size that doesn't match their children", bad)
	}
}

func TestCreate(t *testing.T) {
	e := newEnv(t, nil)
	docs := e.mkdir(t, e.root.ID, "Документы")
	f := e.file(t, docs.ID, "Отчёт.txt", "hello")

	if f.Size != 5 || f.MIME != "text/plain; charset=utf-8" || len(f.ContentHash) != 32 {
		t.Fatalf("file node %+v", f)
	}
	if got, ok := e.onDisk(t, "Документы/Отчёт.txt"); !ok || got != "hello" {
		t.Fatalf("on disk: %q, %v", got, ok)
	}
	root := e.get(t, e.root.ID)
	if root.Size != 5 || root.ETag == e.root.ETag {
		t.Fatalf("root after create: size %d, etag changed %v", root.Size, root.ETag != e.root.ETag)
	}
	if d := e.get(t, docs.ID); d.Size != 5 || d.ETag == docs.ETag || !d.MTime.After(docs.MTime) {
		t.Fatalf("parent after create: %+v", d)
	}
	var used int64
	if err := e.pool.QueryRow(context.Background(), `SELECT used_bytes FROM users WHERE id = $1`, e.user).Scan(&used); err != nil || used != 5 {
		t.Fatalf("used_bytes %d, %v", used, err)
	}
	// Nothing left behind in the staging area.
	if entries, _ := os.ReadDir(filepath.Join(e.dataDir, "uploads")); len(entries) != 0 {
		t.Fatalf("leftovers in uploads: %v", entries)
	}
	e.checkSizes(t)

	// EnsureRoot is idempotent.
	if again, err := e.svc.EnsureRoot(context.Background(), e.user); err != nil || again.ID != e.root.ID {
		t.Fatalf("second EnsureRoot: %v, %v", again.ID, err)
	}
}

func TestNameConflicts(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.mkdir(t, e.root.ID, "Docs")
	e.file(t, e.root.ID, "Café.txt", "x")

	cases := []struct {
		what string
		err  error
	}{
		{"dir, other case", func() error { _, err := e.svc.CreateDir(ctx, e.user, e.root.ID, "DOCS"); return err }()},
		{"file over dir", func() error {
			_, err := e.svc.CreateFile(ctx, e.user, e.root.ID, "docs", strings.NewReader(""), 0)
			return err
		}()},
		{"NFD spelling", func() error {
			_, err := e.svc.CreateFile(ctx, e.user, e.root.ID, "Café.txt", strings.NewReader(""), 0)
			return err
		}()},
	}
	for _, tc := range cases {
		if !errors.Is(tc.err, ErrNameConflict) {
			t.Errorf("%s: err %v, want ErrNameConflict", tc.what, tc.err)
		}
	}
	// A rejected file leaves nothing behind, in storage or in the quota.
	if entries, _ := os.ReadDir(filepath.Join(e.dataDir, "uploads")); len(entries) != 0 {
		t.Fatalf("leftovers in uploads: %v", entries)
	}
	if r := e.get(t, e.root.ID); r.Size != 1 {
		t.Fatalf("root size %d", r.Size)
	}
}

func TestMoveFolder(t *testing.T) {
	e := newEnv(t, nil)
	a := e.mkdir(t, e.root.ID, "a")
	b := e.mkdir(t, a.ID, "b")
	e.file(t, b.ID, "c.txt", "abc")
	x := e.mkdir(t, e.root.ID, "x")
	before := map[string]Node{"root": e.get(t, e.root.ID), "a": e.get(t, a.ID), "b": e.get(t, b.ID), "x": e.get(t, x.ID)}

	moved, err := e.svc.Move(context.Background(), e.user, a.ID, x.ID, "")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if moved.ParentID != x.ID || moved.Name != "a" {
		t.Fatalf("moved %+v", moved)
	}
	if _, ok := e.onDisk(t, "a/b/c.txt"); ok {
		t.Fatal("old path still on disk")
	}
	if got, ok := e.onDisk(t, "x/a/b/c.txt"); !ok || got != "abc" {
		t.Fatalf("new path: %q, %v", got, ok)
	}
	if got := e.get(t, x.ID); got.Size != 3 || got.ETag == before["x"].ETag {
		t.Fatalf("new parent %+v", got)
	}
	if got := e.get(t, e.root.ID); got.Size != 3 || got.ETag == before["root"].ETag {
		t.Fatalf("root %+v", got)
	}
	// Nothing inside the moved folder changes: its content didn't.
	if got := e.get(t, b.ID); got.ETag != before["b"].ETag {
		t.Fatal("etag inside the moved folder changed")
	}
	e.checkSizes(t)
}

func TestMoveRejects(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	a := e.mkdir(t, e.root.ID, "a")
	b := e.mkdir(t, a.ID, "b")
	f := e.file(t, e.root.ID, "f.txt", "x")
	e.mkdir(t, b.ID, "a")

	cases := []struct {
		what             string
		id, parent, name string
		want             error
	}{
		{"into itself", a.ID, a.ID, "", ErrCycle},
		{"into its descendant", a.ID, b.ID, "", ErrCycle},
		{"into a file", a.ID, f.ID, "", ErrNotDir},
		{"root", e.root.ID, a.ID, "", ErrIsRoot},
		{"onto an existing name", b.ID, e.root.ID, "F.TXT", ErrNameConflict},
		{"invalid name", a.ID, e.root.ID, "a/b", ErrInvalidName},
		{"missing node", "00000000-0000-0000-0000-000000000000", e.root.ID, "", ErrNotFound},
	}
	for _, tc := range cases {
		if _, err := e.svc.Move(ctx, e.user, tc.id, tc.parent, tc.name); !errors.Is(err, tc.want) {
			t.Errorf("%s: err %v, want %v", tc.what, err, tc.want)
		}
	}
	if fi, err := os.Stat(filepath.Join(e.dataDir, "files", e.user, "a", "b", "a")); err != nil || !fi.IsDir() {
		t.Fatalf("rejected moves changed the disk: %v", err)
	}
	e.checkSizes(t)
}

func TestRename(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	d := e.mkdir(t, e.root.ID, "docs")
	e.file(t, d.ID, "a.txt", "1")

	for _, name := range []string{"Docs", "Документы"} { // case-only change first
		n, err := e.svc.Rename(ctx, e.user, d.ID, name)
		if err != nil || n.Name != name {
			t.Fatalf("rename to %q: %+v, %v", name, n, err)
		}
		if _, ok := e.onDisk(t, name+"/a.txt"); !ok {
			t.Fatalf("%q not on disk", name)
		}
	}
	if _, err := e.svc.Rename(ctx, e.user, e.root.ID, "x"); !errors.Is(err, ErrIsRoot) {
		t.Fatalf("rename root: %v", err)
	}
	// Renaming to the current name is a no-op, not a conflict.
	if _, err := e.svc.Rename(ctx, e.user, d.ID, "Документы"); err != nil {
		t.Fatalf("rename to itself: %v", err)
	}
}

func TestForeignNodesAreInvisible(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	mine := e.mkdir(t, e.root.ID, "mine")

	bob := e.newUser(t, "bob")
	bobRoot, err := e.svc.EnsureRoot(ctx, bob)
	if err != nil {
		t.Fatalf("bob root: %v", err)
	}
	attempts := map[string]error{
		"get":       func() error { _, err := e.svc.Get(ctx, bob, mine.ID); return err }(),
		"mkdir in":  func() error { _, err := e.svc.CreateDir(ctx, bob, mine.ID, "x"); return err }(),
		"upload in": func() error { _, err := e.svc.CreateFile(ctx, bob, mine.ID, "x", strings.NewReader(""), 0); return err }(),
		"rename":    func() error { _, err := e.svc.Rename(ctx, bob, mine.ID, "x"); return err }(),
		"move away": func() error { _, err := e.svc.Move(ctx, bob, mine.ID, bobRoot.ID, ""); return err }(),
		"move in":   func() error { _, err := e.svc.Move(ctx, e.user, mine.ID, bobRoot.ID, ""); return err }(),
	}
	for what, err := range attempts {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err %v, want ErrNotFound", what, err)
		}
	}
}

// failingStore fails the storage step that comes last inside a transaction,
// to check the transaction is rolled back with it.
type failingStore struct {
	storage.Backend
}

var errDisk = errors.New("disk on fire")

func (failingStore) Mkdir(_ context.Context, key string) error {
	if strings.Count(key, "/") > 1 { // let EnsureRoot's files/{user} through
		return errDisk
	}
	return nil
}

func (f failingStore) Move(ctx context.Context, from, to string) error {
	if strings.HasPrefix(from, "uploads/") {
		return f.Backend.Move(ctx, from, to)
	}
	return errDisk
}

func TestStorageFailureRollsBack(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	d := e.mkdir(t, e.root.ID, "d")
	rootBefore := e.get(t, e.root.ID)

	failing := New(e.pool, failingStore{e.svc.store})
	if _, err := failing.CreateDir(ctx, e.user, e.root.ID, "new"); !errors.Is(err, errDisk) {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := failing.Rename(ctx, e.user, d.ID, "renamed"); !errors.Is(err, errDisk) {
		t.Fatalf("rename: %v", err)
	}
	if got := e.get(t, d.ID); got.Name != "d" {
		t.Fatalf("rename survived the storage failure: %+v", got)
	}
	if got := e.get(t, e.root.ID); got.ETag != rootBefore.ETag {
		t.Fatal("root etag changed by a rolled-back operation")
	}
	// The failed mkdir's name is free again.
	e.mkdir(t, e.root.ID, "new")
}

type flatStore struct{ storage.Backend }

func (flatStore) Layout() storage.Layout { return storage.LayoutFlat }

func TestFlatLayout(t *testing.T) {
	e := newEnv(t, func(b storage.Backend) storage.Backend { return flatStore{b} })
	ctx := context.Background()
	d := e.mkdir(t, e.root.ID, "d")
	f := e.file(t, d.ID, "a.txt", "flat")

	if _, ok := e.onDisk(t, "d/a.txt"); ok {
		t.Fatal("flat layout wrote into the tree")
	}
	var key string
	if err := e.pool.QueryRow(ctx, `SELECT storage_key FROM blobs WHERE id = $1`, f.blobID).Scan(&key); err != nil {
		t.Fatalf("blob: %v", err)
	}
	if key != "blobs/"+f.blobID {
		t.Fatalf("storage_key %q", key)
	}
	if _, err := e.svc.Rename(ctx, e.user, d.ID, "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(e.dataDir, "blobs", f.blobID))
	if err != nil || string(data) != "flat" {
		t.Fatalf("object after rename: %q, %v", data, err)
	}
}

// TestConcurrentWrites runs uploads and moves over the same folders at
// once: they must neither deadlock nor lose a size update.
func TestConcurrentWrites(t *testing.T) {
	e := newEnv(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const dirs, perDir = 4, 10
	ids := make([]string, dirs)
	for i := range ids {
		parent := e.root.ID
		if i > 0 {
			parent = ids[i-1] // a chain, so every operation shares ancestors
		}
		ids[i] = e.mkdir(t, parent, fmt.Sprintf("d%d", i)).ID
	}

	var wg sync.WaitGroup
	errs := make(chan error, dirs*perDir*2)
	for i := range dirs {
		for j := range perDir {
			wg.Go(func() {
				name := fmt.Sprintf("f%d-%d", i, j)
				n, err := e.svc.CreateFile(ctx, e.user, ids[i], name, strings.NewReader(strings.Repeat("x", j+1)), int64(j+1))
				if err != nil {
					errs <- fmt.Errorf("create %s: %w", name, err)
					return
				}
				// Move it to a sibling level, across other writers.
				if _, err := e.svc.Move(ctx, e.user, n.ID, ids[(i+1)%dirs], ""); err != nil {
					errs <- fmt.Errorf("move %s: %w", name, err)
				}
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	e.checkSizes(t)
	want := int64(dirs * perDir * (perDir + 1) / 2)
	if r := e.get(t, e.root.ID); r.Size != want {
		t.Fatalf("root size %d, want %d", r.Size, want)
	}
}

func (e env) read(t *testing.T, id string, rng *storage.Range) string {
	t.Helper()
	rc, _, err := e.svc.Open(context.Background(), e.user, id, rng)
	if err != nil {
		t.Fatalf("open %s: %v", id, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return string(data)
}

func TestOpen(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	d := e.mkdir(t, e.root.ID, "Документы")
	f := e.file(t, d.ID, "a.txt", "0123456789")

	if got := e.read(t, f.ID, nil); got != "0123456789" {
		t.Fatalf("content %q", got)
	}
	if got := e.read(t, f.ID, &storage.Range{Offset: 3, Length: 4}); got != "3456" {
		t.Fatalf("range %q", got)
	}
	if _, _, err := e.svc.Open(ctx, e.user, d.ID, nil); !errors.Is(err, ErrNotFile) {
		t.Fatalf("open dir: %v", err)
	}
	bob := e.newUser(t, "bob")
	if _, _, err := e.svc.Open(ctx, bob, f.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open foreign: %v", err)
	}
	// After a move the same id still opens: the path is resolved per call.
	if _, err := e.svc.Rename(ctx, e.user, d.ID, "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := e.read(t, f.ID, nil); got != "0123456789" {
		t.Fatalf("content after rename %q", got)
	}
}

// OpenRange hands pick the node it opens, and pick's error stops it
// before anything is opened.
func TestOpenRange(t *testing.T) {
	for name, wrap := range map[string]func(storage.Backend) storage.Backend{
		"tree": nil,
		"flat": func(b storage.Backend) storage.Backend { return flatStore{b} },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, wrap)
			ctx := context.Background()
			f := e.file(t, e.root.ID, "a.txt", "0123456789")
			rc, n, err := e.svc.OpenRange(ctx, e.user, f.ID, func(n Node) (*storage.Range, error) {
				return &storage.Range{Offset: n.Size - 2, Length: 2}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			if string(data) != "89" || n.ID != f.ID {
				t.Fatalf("read %q of %+v", data, n)
			}
			stop := errors.New("stop")
			if rc, _, err := e.svc.OpenRange(ctx, e.user, f.ID, func(Node) (*storage.Range, error) { return nil, stop }); !errors.Is(err, stop) || rc != nil {
				t.Fatalf("pick error: %v, %v", rc, err)
			}
			if _, _, err := e.svc.OpenRange(ctx, e.user, "not-a-uuid", nil); !errors.Is(err, ErrNotFound) {
				t.Fatalf("malformed id: %v", err)
			}
		})
	}
}

func TestOpenFlat(t *testing.T) {
	e := newEnv(t, func(b storage.Backend) storage.Backend { return flatStore{b} })
	f := e.file(t, e.root.ID, "a.txt", "flat")
	if got := e.read(t, f.ID, nil); got != "flat" {
		t.Fatalf("content %q", got)
	}
}

// TestOpenDuringRenames reads a file while its grandparent is renamed back
// and forth: without the path locks a read resolves the old path and finds
// nothing there (risk A2).
func TestOpenDuringRenames(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	a := e.mkdir(t, e.root.ID, "a")
	b := e.mkdir(t, a.ID, "b")
	f := e.file(t, b.ID, "f.txt", "payload")

	stop := make(chan struct{})
	var renames, reads, failures atomic.Int64
	var wg sync.WaitGroup
	wg.Go(func() {
		names := []string{"a2", "a"}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := e.svc.Rename(ctx, e.user, a.ID, names[i%2]); err != nil {
				t.Errorf("rename: %v", err)
				return
			}
			renames.Add(1)
		}
	})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				rc, _, err := e.svc.Open(ctx, e.user, f.ID, nil)
				if err != nil {
					failures.Add(1)
					continue
				}
				data, err := io.ReadAll(rc)
				rc.Close()
				if err != nil || string(data) != "payload" {
					failures.Add(1)
					continue
				}
				reads.Add(1)
			}
		})
	}
	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()

	t.Logf("%d renames, %d reads", renames.Load(), reads.Load())
	if renames.Load() < 10 || reads.Load() < 10 {
		t.Fatalf("too little contention to mean anything: %d renames, %d reads", renames.Load(), reads.Load())
	}
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d reads failed while an ancestor was being renamed", n)
	}
}
