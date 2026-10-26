package preview

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/db"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/storage/localfs"
)

func testDatabaseURL() string {
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
}

type env struct {
	svc     *Service
	files   *fs.Service
	pool    *pgxpool.Pool
	dataDir string
	alice   string
	bob     string
	root    fs.Node
	runs    string
}

func newEnv(t *testing.T, vips string) env {
	t.Helper()
	url := testDatabaseURL()
	if err := db.Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, url, 4, 0, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE users, nodes, blobs, versions CASCADE`); err != nil {
		t.Fatal(err)
	}
	e := env{pool: pool, dataDir: t.TempDir(), runs: filepath.Join(t.TempDir(), "runs")}
	t.Setenv("FAKE_VIPS_RUNS", e.runs)
	store, err := localfs.Open(e.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e.files = fs.New(pool, store)
	for _, name := range []string{"alice", "bob"} {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO users (username) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		root, err := e.files.EnsureRoot(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if name == "alice" {
			e.alice, e.root = id, root
		} else {
			e.bob = id
		}
	}
	e.svc = &Service{files: e.files, store: store, db: pool, vips: vips, slots: make(chan struct{}, 2)}
	return e
}

func fake(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("testdata/fake-vips")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e env) runCount(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(e.runs)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "run")
}

func (e env) read(t *testing.T, actor, id string, size Size) (string, error) {
	t.Helper()
	rc, _, err := e.svc.Open(context.Background(), actor, id, size)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return string(b), err
}

func TestMadeOnceAndKept(t *testing.T) {
	e := newEnv(t, fake(t))
	ctx := context.Background()
	f, err := e.files.CreateFile(ctx, e.alice, e.root.ID, "фото.JPG", strings.NewReader("jpeg-bytes"), 10)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got, err := e.read(t, e.alice, f.ID, Small); err != nil || got != "WEBP:256:jpeg-bytes" {
			t.Fatalf("small = %q, %v", got, err)
		}
	}
	if got, err := e.read(t, e.alice, f.ID, Large); err != nil || got != "WEBP:2048:jpeg-bytes" {
		t.Fatalf("large = %q, %v", got, err)
	}
	if n := e.runCount(t); n != 2 {
		t.Fatalf("vips ran %d times, want once per size", n)
	}
	// Whoever can't read the file sees no preview of it either.
	if _, err := e.read(t, e.bob, f.ID, Small); !errors.Is(err, fs.ErrNotFound) {
		t.Fatalf("someone else: %v", err)
	}

	// A new version is new content: a new preview.
	if _, _, err := e.files.PutFile(ctx, e.alice, e.root.ID, "фото.JPG", strings.NewReader("v2"), 2, fs.PutCond{}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.read(t, e.alice, f.ID, Small); err != nil || got != "WEBP:256:v2" {
		t.Fatalf("after a new version = %q, %v", got, err)
	}

	// Purged with its blobs: the rows and the files.
	if err := e.files.Trash(ctx, e.alice, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.files.Purge(ctx, e.alice, f.ID); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM previews`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("previews rows left: %d, %v", rows, err)
	}
	if left, _ := os.ReadDir(filepath.Join(e.dataDir, "previews")); len(left) != 0 {
		t.Fatalf("preview files left: %v", left)
	}
}

func TestNoPreview(t *testing.T) {
	e := newEnv(t, fake(t))
	ctx := context.Background()
	bad, _ := e.files.CreateFile(ctx, e.alice, e.root.ID, "broken.png", strings.NewReader("BAD"), 3)
	for range 2 {
		if _, err := e.read(t, e.alice, bad.ID, Small); !errors.Is(err, ErrNone) {
			t.Fatalf("broken file: %v", err)
		}
	}
	if n := e.runCount(t); n != 1 {
		t.Fatalf("a failure is tried %d times, want once", n)
	}
	text, _ := e.files.CreateFile(ctx, e.alice, e.root.ID, "notes.txt", strings.NewReader("x"), 1)
	empty, _ := e.files.CreateFile(ctx, e.alice, e.root.ID, "empty.png", strings.NewReader(""), 0)
	dir, _ := e.files.CreateDir(ctx, e.alice, e.root.ID, "photos.jpg")
	for _, id := range []string{text.ID, empty.ID, dir.ID} {
		if _, err := e.read(t, e.alice, id, Small); !errors.Is(err, ErrNone) {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if _, err := e.read(t, e.alice, text.ID, "huge"); !errors.Is(err, ErrNone) {
		t.Fatalf("unknown size: %v", err)
	}
	if n := e.runCount(t); n != 1 {
		t.Fatalf("vips ran for files it can't preview: %d", n)
	}
	// Without vips, nothing at all.
	off := newEnv(t, "")
	f, _ := off.files.CreateFile(ctx, off.alice, off.root.ID, "a.png", strings.NewReader("x"), 1)
	if _, err := off.read(t, off.alice, f.ID, Small); !errors.Is(err, ErrNone) || off.svc.Enabled() {
		t.Fatalf("without vips: %v", err)
	}
}

// With the real vips, where it is installed (the Docker image).
func TestRealVips(t *testing.T) {
	vips, err := exec.LookPath("vips")
	if err != nil {
		t.Skip("vips not installed")
	}
	e := newEnv(t, vips)
	img := image.NewRGBA(image.Rect(0, 0, 600, 300))
	for x := range 600 {
		for y := range 300 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 120, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	f, err := e.files.CreateFile(context.Background(), e.alice, e.root.ID, "a.png", bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.read(t, e.alice, f.ID, Small)
	if err != nil || !strings.HasPrefix(got, "RIFF") || got[8:12] != "WEBP" {
		t.Fatalf("real preview: %q…, %v", got[:min(len(got), 16)], err)
	}
	// Never enlarged: a 600 px wide image stays 600 at size l.
	large, err := e.read(t, e.alice, f.ID, Large)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := webpWidth([]byte(large))
	if err != nil || cfg != 600 {
		t.Fatalf("large width %d, %v", cfg, err)
	}
}

// webpWidth reads a lossy WebP's width from its VP8 frame header.
func webpWidth(b []byte) (int, error) {
	i := bytes.Index(b, []byte{0x9d, 0x01, 0x2a})
	if i < 0 || i+5 > len(b) {
		return 0, errors.New("not a lossy webp")
	}
	return int(b[i+3]) | int(b[i+4]&0x3f)<<8, nil
}
