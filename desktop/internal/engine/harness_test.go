package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// tb is what the harness needs of testing.T and rapid.T.
type tb interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

// harness is one client (sync folder, state database, engine) against a
// Remote.
type harness struct {
	t      tb
	root   string
	dbPath string
	db     *state.DB
	r      Remote
	e      *Engine
	cfg    Config
	logs   *logTail
}

// logTail keeps the engine's last log lines for a failing test.
type logTail struct {
	mu    sync.Mutex
	lines []string
}

func (l *logTail) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimRight(string(p), "\n"))
	if len(l.lines) > 200 {
		l.lines = l.lines[len(l.lines)-200:]
	}
	return len(p), nil
}

func (l *logTail) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

const testConn = "conn-test"

func newHarness(t testing.TB, r Remote, userID string, tune func(*Config)) *harness {
	t.Helper()
	return newHarnessIn(t, t.TempDir(), r, userID, tune)
}

func newHarnessIn(t tb, dir string, r Remote, userID string, tune func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, root: filepath.Join(dir, "Облако"), dbPath: filepath.Join(dir, "state.db"), r: r, logs: &logTail{}}
	if err := os.Mkdir(h.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := localfs.CreateMarker(h.root, testConn); err != nil {
		t.Fatal(err)
	}
	h.cfg = Config{Remote: r, Root: h.root, ConnID: testConn, UserID: userID,
		Options:      Options{DeviceName: "Тест", StableAfter: 0},
		PollInterval: 50 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if os.Getenv("ENGINE_LOG") != "" {
		h.cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if tune != nil {
		tune(&h.cfg)
	}
	h.open()
	return h
}

// open (re)opens the database and makes a new engine on it, as a restart
// of the process does.
func (h *harness) open() {
	h.t.Helper()
	db, err := state.Open(context.Background(), h.dbPath)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := db.Update(context.Background(), func(tx *state.Tx) error {
		if _, err := tx.Account(); err == nil {
			return nil
		}
		return tx.SetAccount(state.Account{Server: "http://fake", UserID: h.cfg.UserID, DeviceID: "d", LocalRoot: h.root})
	}); err != nil {
		h.t.Fatal(err)
	}
	h.db = db
	cfg := h.cfg
	cfg.DB = db
	e, err := New(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	h.e = e
}

// restart closes the database and opens a fresh engine.
func (h *harness) restart() {
	h.db.Close()
	h.open()
}

func (h *harness) close() { h.db.Close() }

func (h *harness) sync() PassResult {
	h.t.Helper()
	res, err := h.e.SyncOnce(context.Background())
	if err != nil {
		h.t.Fatalf("sync: %v", err)
	}
	return res
}

func (h *harness) selectAll() {
	h.t.Helper()
	ctx := context.Background()
	if err := h.e.Prepare(ctx); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.e.Select(ctx, "/"); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) write(rel, content string) {
	h.t.Helper()
	p := filepath.Join(h.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) read(rel string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.root, filepath.FromSlash(rel)))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func (h *harness) problems() []state.Problem {
	var ps []state.Problem
	_ = h.db.View(context.Background(), func(tx *state.Tx) error {
		var err error
		ps, err = tx.Problems()
		return err
	})
	return ps
}

func shaOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// localTree is the sync folder as path → sha256 ("/" for folders),
// without what sync ignores.
func localTree(t tb, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		if localfs.Ignored(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out[rel] = "/"
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = shaOf(string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// keyed compares trees as the server compares names.
func keyed(m map[string]string) map[string]string {
	out := map[string]string{}
	for p, v := range m {
		parts := strings.Split(p, "/")
		for i := range parts {
			parts[i] = localfs.NameKey(parts[i])
		}
		out[strings.Join(parts, "/")] = v
	}
	return out
}

func diffTrees(a, b map[string]string) string {
	a, b = keyed(a), keyed(b)
	var out []string
	for p, v := range a {
		if w, ok := b[p]; !ok {
			out = append(out, "only local:  "+p)
		} else if w != v {
			out = append(out, "differs:     "+p)
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			out = append(out, "only server: "+p)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// dump is the state and the plan it makes now, for a failing test.
func (h *harness) dump() string {
	ctx := context.Background()
	snap, _, err := h.e.snapshot(ctx)
	if err != nil {
		return err.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "root %s\nremote:\n", snap.RootID)
	for _, r := range snap.Remote {
		fmt.Fprintf(&b, "  %s parent=%s %q %s etag=%s sha=%.8s\n", r.NodeID, r.ParentID, r.Name, r.Kind, r.ETag, r.SHA256)
	}
	b.WriteString("base:\n")
	for _, x := range snap.Base {
		fmt.Fprintf(&b, "  %s %q %s etag=%s sha=%.8s fid=%s\n", x.NodeID, x.RelPath, x.Kind, x.ETag, x.SHA256, x.FileID)
	}
	b.WriteString("local:\n")
	for _, l := range snap.Local {
		fmt.Fprintf(&b, "  %q %s fid=%s sha=%.8s\n", l.RelPath, l.Kind, l.FileID, l.SHA256Cached)
	}
	fmt.Fprintf(&b, "busy %v %v\n", snap.BusyNodes, snap.BusyPaths)
	p := MakePlan(snap)
	fmt.Fprintf(&b, "plan (deferred %d):\n%s\nlog:\n%s\n", p.Deferred, ops(p), h.logs)
	return b.String()
}
