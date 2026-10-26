package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The engine against the fake server: the basic flows end to end, fast.

func fakeHarness(t *testing.T) (*harness, *fakeServer) {
	s := newFakeServer()
	h := newHarness(t, s, "u1", nil)
	t.Cleanup(h.close)
	return h, s
}

func expectSame(t *testing.T, h *harness, s *fakeServer) {
	t.Helper()
	if d := diffTrees(localTree(t, h.root), s.tree()); d != "" {
		t.Fatalf("local and server differ:\n%s", d)
	}
}

func TestEngineBothWays(t *testing.T) {
	h, s := fakeHarness(t)
	s.put("Docs/a.txt", "a1")
	s.put("top.txt", "t")
	h.sync()
	if got := localTree(t, h.root); len(got) != 0 {
		t.Fatalf("synced before anything was selected: %v", got)
	}
	h.selectAll()
	h.sync()
	expectSame(t, h, s)

	h.write("Docs/b.txt", "b1")
	h.write("New/deep/c.txt", "c1")
	h.sync()
	expectSame(t, h, s)

	// Edits, renames, moves and deletes both ways.
	h.write("Docs/a.txt", "a2")
	s.put("top.txt", "t2")
	if err := os.Rename(filepath.Join(h.root, "New"), filepath.Join(h.root, "Renamed")); err != nil {
		t.Fatal(err)
	}
	h.sync()
	expectSame(t, h, s)
	if h.read("top.txt") != "t2" {
		t.Fatal("remote edit not downloaded")
	}
	ids := map[string]string{}
	for p, id := range s.paths() {
		ids[p] = id
	}
	if _, err := s.move(ids["Docs/b.txt"], ids["Renamed"], "moved-b.txt", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.trash(ids["top.txt"], ""); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(h.root, "Docs", "a.txt"))
	h.sync()
	expectSame(t, h, s)
	if _, err := os.Stat(filepath.Join(h.root, "Renamed", "moved-b.txt")); err != nil {
		t.Fatal(err)
	}
	if s.live(ids["Docs/a.txt"]) != nil {
		t.Fatal("local delete didn't trash on the server")
	}
	// Converged: one more pass does nothing.
	if res, err := h.e.Pass(context.Background()); err != nil || res.Ops != 0 {
		t.Fatalf("extra pass: %+v, %v", res, err)
	}
}

func TestEngineConflict(t *testing.T) {
	h, s := fakeHarness(t)
	s.put("doc.txt", "v1")
	h.selectAll()
	h.sync()
	h.write("doc.txt", "local")
	s.put("doc.txt", "server")
	h.sync()
	expectSame(t, h, s)
	if h.read("doc.txt") != "server" {
		t.Fatalf("the server's version doesn't keep the name: %q", h.read("doc.txt"))
	}
	var copyName string
	for p := range s.tree() {
		if strings.Contains(p, "конфликт, Тест,") {
			copyName = p
		}
	}
	if copyName == "" || h.read(copyName) != "local" {
		t.Fatalf("no conflict copy on the server: %v", s.tree())
	}
	if ps := h.problems(); len(ps) != 1 || ps[0].Kind != ProblemConflict {
		t.Fatalf("problems %+v", ps)
	}
}

func TestEngineNoMarker(t *testing.T) {
	h, s := fakeHarness(t)
	s.put("a.txt", "a")
	h.selectAll()
	h.sync()
	// The disk "unplugged": an empty folder where the sync folder was.
	moved := h.root + ".away"
	if err := os.Rename(h.root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.root, 0o755); err != nil {
		t.Fatal(err)
	}
	seq := s.seq
	if _, err := h.e.Pass(context.Background()); !errors.Is(err, ErrNoMarker) {
		t.Fatalf("pass without the marker: %v", err)
	}
	if s.seq != seq || s.live(s.paths()["a.txt"]) == nil {
		t.Fatal("something changed on the server without the marker")
	}
	os.Remove(h.root)
	if err := os.Rename(moved, h.root); err != nil {
		t.Fatal(err)
	}
	h.sync()
	expectSame(t, h, s)
}

func TestEngineMassDeleteGuard(t *testing.T) {
	h, s := fakeHarness(t)
	for i := range 20 {
		s.put("many/"+string(rune('a'+i))+".txt", "x")
	}
	h.selectAll()
	h.sync()
	if err := os.RemoveAll(filepath.Join(h.root, "many")); err != nil {
		t.Fatal(err)
	}
	res := h.sync()
	if res.HeldDeletes != 20 || len(s.tree()) != 21 {
		t.Fatalf("held %d, server %d entries", res.HeldDeletes, len(s.tree()))
	}
	if ps := h.problems(); len(ps) != 1 || ps[0].Kind != ProblemMassDelete {
		t.Fatalf("problems %+v", ps)
	}
	h.e.cfg.ConfirmDeletes = true
	h.sync()
	expectSame(t, h, s)
	if len(h.problems()) != 0 {
		t.Fatalf("problems after confirming: %+v", h.problems())
	}
}

func TestEngineCursorExpired(t *testing.T) {
	h, s := fakeHarness(t)
	s.put("a.txt", "a")
	s.put("d/b.txt", "b")
	h.selectAll()
	h.sync()
	s.put("d/c.txt", "c")
	s.expire()
	calls := 0
	s.fault = func(m string) (bool, bool) {
		if m == "download" {
			calls++
		}
		return false, false
	}
	h.sync()
	expectSame(t, h, s)
	if calls != 1 {
		t.Fatalf("%d downloads after the resync, want 1 (only the new file)", calls)
	}
}

// A crash between an action and its result: every op kind is redone on
// the next start without loss or duplicates.
func TestEngineCrashEveryOp(t *testing.T) {
	for _, stage := range []string{"act", "acted"} {
		for _, kind := range []OpKind{OpUpload, OpDownload, OpMkdirRemote, OpMkdirLocal, OpMoveRemote, OpRenameLocal, OpTrashRemote, OpRemoveLocal, OpConflict} {
			t.Run(stage+"/"+string(kind), func(t *testing.T) {
				s := newFakeServer()
				var crashed atomic.Bool // armed after the setup
				crashed.Store(true)
				h := newHarness(t, s, "u1", func(c *Config) {
					c.Hook = func(st string, o Op) error {
						if st == stage && o.Kind == kind && crashed.CompareAndSwap(false, true) {
							return errStop
						}
						return nil
					}
				})
				defer h.close()
				s.put("keep/x.txt", "x")
				s.put("srv/del.txt", "d")
				s.put("srv/edit.txt", "e1")
				s.put("srv/mv.txt", "m")
				s.put("srv/local-del.txt", "ld")
				h.selectAll()
				h.sync()
				crashed.Store(false)
				h.write("local/new.txt", "n")
				h.write("keep/x.txt", "x-local")
				s.put("keep/x.txt", "x-server")
				s.put("srv/edit.txt", "e2")
				s.put("srv/added/y.txt", "y")
				ids := s.paths()
				_, _ = s.move(ids["srv/mv.txt"], ids["srv"], "mv2.txt", "")
				_ = s.trash(ids["srv/del.txt"], "")
				os.Rename(filepath.Join(h.root, "keep"), filepath.Join(h.root, "keep2"))
				os.Remove(filepath.Join(h.root, "srv", "local-del.txt"))
				for range 10 {
					_, err := h.e.SyncOnce(context.Background())
					if err == nil {
						break
					}
					if !crashed.Load() {
						t.Fatalf("failed without the crash: %v", err)
					}
					if !errors.Is(err, errStop) {
						t.Fatal(err)
					}
					h.restart()
				}
				if !crashed.Load() {
					t.Fatal("no op of this kind ran")
				}
				h.sync()
				expectSame(t, h, s)
				if res, err := h.e.Pass(context.Background()); err != nil || res.Ops != 0 {
					t.Fatalf("extra pass: %+v, %v", res, err)
				}
				for _, c := range []string{"x-local", "x-server", "n", "e2", "y", "m"} {
					found := false
					for _, v := range s.tree() {
						found = found || v == shaOf(c)
					}
					if !found {
						t.Errorf("%q is not on the server", c)
					}
				}
			})
		}
	}
}

// paths maps each live node's path to its id.
func (s *fakeServer) paths() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	var walk func(id, prefix string)
	walk = func(id, prefix string) {
		for _, c := range s.nodes {
			if c.parent == id && !c.trashed && c.id != s.rootID {
				p := join(prefix, c.name)
				out[p] = c.id
				if c.dir {
					walk(c.id, p)
				}
			}
		}
	}
	walk(s.rootID, "")
	return out
}
