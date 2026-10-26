package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/do-service/cloud/backend/internal/db"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/storage/localfs"
)

type env struct {
	pool    *pgxpool.Pool
	files   *fs.Service
	dataDir string
	user    string
	root    fs.Node
}

func newEnv(t *testing.T) env {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
	}
	if err := db.Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, url, 8, 0, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE users, nodes, blobs, versions, uploads CASCADE`); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	store, err := localfs.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e := env{pool: pool, files: fs.New(pool, store), dataDir: dataDir}
	if err := pool.QueryRow(ctx, `INSERT INTO users (username) VALUES ('alice') RETURNING id`).Scan(&e.user); err != nil {
		t.Fatal(err)
	}
	if e.root, err = e.files.EnsureRoot(ctx, e.user); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e env) file(t *testing.T, name, content string) fs.Node {
	t.Helper()
	n, err := e.files.CreateFile(context.Background(), e.user, e.root.ID, name, strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTrashRetention(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	old, fresh := e.file(t, "old.txt", "o"), e.file(t, "fresh.txt", "f")
	for _, n := range []fs.Node{old, fresh} {
		if err := e.files.Trash(ctx, e.user, n.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.pool.Exec(ctx, `UPDATE nodes SET trashed_at = now() - interval '31 days' WHERE id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}
	w := &TrashWorker{FS: e.files, Retention: 30 * 24 * time.Hour}
	if err := w.Work(ctx, nil); err != nil {
		t.Fatal(err)
	}
	items, _, err := e.files.ListTrash(ctx, e.user, 0, "")
	if err != nil || len(items) != 1 || items[0].ID != fresh.ID {
		t.Fatalf("trash after retention: %+v, %v", items, err)
	}
	// Restoring the purged one is no longer possible.
	if _, err := e.files.Restore(ctx, e.user, old.ID); !errors.Is(err, fs.ErrNotFound) {
		t.Fatalf("restore purged: %v", err)
	}
}

// replace gives a.txt new content the way an upload with
// onConflict=replace does, keeping the old content as a version.
func (e env) replace(t *testing.T, name, content string) {
	t.Helper()
	ctx := context.Background()
	up, err := e.files.ReserveUpload(ctx, e.user, fs.Upload{ParentID: e.root.ID, Name: name, Size: int64(len(content)), OnConflict: fs.ConflictReplace})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.dataDir, "uploads", "tus")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, up.ID), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := e.files.FinishUpload(ctx, e.user, up.ID, "uploads/tus/"+up.ID); err != nil {
		t.Fatal(err)
	}
}

// The worker keeps calling fs until a batch comes back short, so a backlog
// bigger than one batch is cleared in one run.
func TestVersionPolicy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f := e.file(t, "a.txt", "v0")
	for i := 1; i <= 7; i++ {
		e.replace(t, "a.txt", "v"+strconv.Itoa(i))
	}
	defer func(old int) { batch = old }(batch)
	batch = 2
	w := &VersionsWorker{FS: e.files, Policy: fs.VersionPolicy{MaxCount: 2}}
	if err := w.Work(ctx, nil); err != nil {
		t.Fatal(err)
	}
	versions, _, err := e.files.ListVersions(ctx, e.user, f.ID)
	if err != nil || len(versions) != 2 {
		t.Fatalf("versions left: %d, %v", len(versions), err)
	}
}

func TestSetup(t *testing.T) {
	workers := river.NewWorkers()
	if p := Setup(workers, nil, 0, fs.VersionPolicy{}); len(p) != 0 {
		t.Fatalf("disabled policies scheduled %d jobs", len(p))
	}
	if p := Setup(river.NewWorkers(), nil, time.Hour, fs.VersionPolicy{MaxCount: 3}); len(p) != 2 {
		t.Fatalf("enabled policies scheduled %d jobs", len(p))
	}
}

// Rows past the retention go, in as many batches as it takes, and the
// tree's min_seq moves up to the last of them.
func TestJournalRetention(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := range 5 {
		e.file(t, "f"+strconv.Itoa(i)+".txt", "x")
	}
	fresh := e.file(t, "fresh.txt", "x")
	if _, err := e.pool.Exec(ctx, `UPDATE sync_journal SET at = now() - interval '31 days' WHERE owner_id = $1 AND node_id <> $2`, e.user, fresh.ID); err != nil {
		t.Fatal(err)
	}
	defer func(old int) { batch = old }(batch)
	batch = 2
	w := &JournalWorker{FS: e.files, Retention: 30 * 24 * time.Hour}
	if err := w.Work(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var left []int64
	rows, err := e.pool.Query(ctx, `SELECT seq FROM sync_journal WHERE owner_id = $1`, e.user)
	if err == nil {
		left, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	}
	var minSeq, treeSeq int64
	if err == nil {
		err = e.pool.QueryRow(ctx, `SELECT min_seq, tree_seq FROM sync_heads WHERE user_id = $1`, e.user).Scan(&minSeq, &treeSeq)
	}
	if err != nil || len(left) != 1 || left[0] != treeSeq || minSeq != treeSeq-1 {
		t.Fatalf("after retention: rows %v, min_seq %d, tree_seq %d, %v", left, minSeq, treeSeq, err)
	}
	if p := SetupJournal(river.NewWorkers(), nil, 0); len(p) != 0 {
		t.Fatalf("retention 0 scheduled %d jobs", len(p))
	}
	if p := SetupJournal(river.NewWorkers(), e.files, time.Hour); len(p) != 1 {
		t.Fatalf("retention scheduled %d jobs", len(p))
	}
}
