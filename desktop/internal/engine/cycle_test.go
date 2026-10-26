package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/do-service/cloud/desktop/internal/localfs"
)

// Cycles of renames: a and b swapped (through a third name) on one side,
// which the other side can't follow one rename at a time — each name is
// taken by the other node. One node steps aside under a temporary name
// (tempName) on the side that follows, the rest go, and it takes its name
// last; a crash anywhere in between converges on the next start.

type cycleCase struct {
	name  string
	files map[string]string
	// steps renames, in order (from, to); "+dir" creates dir.
	steps [][2]string
	// moved: where each original path's node ends.
	moved map[string]string
}

var cycleCases = []cycleCase{
	{
		name:  "two files",
		files: map[string]string{"a.txt": "A", "b.txt": "B"},
		steps: [][2]string{{"a.txt", "t"}, {"b.txt", "a.txt"}, {"t", "b.txt"}},
		moved: map[string]string{"a.txt": "b.txt", "b.txt": "a.txt"},
	},
	{
		name:  "two folders",
		files: map[string]string{"a/x.txt": "A", "b/y.txt": "B"},
		steps: [][2]string{{"a", "t"}, {"b", "a"}, {"t", "b"}},
		moved: map[string]string{"a": "b", "b": "a", "a/x.txt": "b/x.txt", "b/y.txt": "a/y.txt"},
	},
	{
		name:  "three",
		files: map[string]string{"a": "A", "b": "B", "c": "C"},
		steps: [][2]string{{"a", "t"}, {"c", "a"}, {"b", "c"}, {"t", "b"}},
		moved: map[string]string{"a": "b", "b": "c", "c": "a"},
	},
	{
		name:  "across folders",
		files: map[string]string{"x/a.txt": "A", "y/b.txt": "B"},
		steps: [][2]string{{"x/a.txt", "t"}, {"y/b.txt", "x/a.txt"}, {"t", "y/b.txt"}},
		moved: map[string]string{"x/a.txt": "y/b.txt", "y/b.txt": "x/a.txt"},
	},
	{
		// A folder takes another's name, and that one goes into a new
		// folder of the first one's old name.
		name:  "into a new folder",
		files: map[string]string{"d/1.txt": "1", "e/2.txt": "2"},
		steps: [][2]string{{"e", "t"}, {"d", "e"}, {"+dir", "d"}, {"t", "d/e"}},
		moved: map[string]string{"d": "e", "e": "d/e", "d/1.txt": "e/1.txt", "e/2.txt": "d/e/2.txt"},
	},
}

func (c cycleCase) apply(t *testing.T, root string) {
	t.Helper()
	abs := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }
	for _, st := range c.steps {
		var err error
		if st[0] == "+dir" {
			err = os.Mkdir(abs(st[1]), 0o755)
		} else {
			err = os.Rename(abs(st[0]), abs(st[1]))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// applyRemote makes the same renames on the server.
func (c cycleCase) applyRemote(t *testing.T, s *fakeServer) {
	t.Helper()
	for _, st := range c.steps {
		s.mu.Lock()
		var err error
		if st[0] == "+dir" {
			_, err = s.mkdir(s.rootID, st[1])
		} else {
			ids := s.pathsLocked()
			parent := s.rootID
			if d := dirOf(st[1]); d != "" {
				parent = ids[d]
			}
			_, err = s.move(ids[st[0]], parent, nameOf(st[1]), "")
		}
		s.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
}

// pathsLocked is paths with s.mu held.
func (s *fakeServer) pathsLocked() map[string]string {
	s.mu.Unlock()
	defer s.mu.Lock()
	return s.paths()
}

// checkCycle: both sides agree, the nodes moved (not uploaded again), no
// temporary name is left, and nothing more to do.
func checkCycle(t *testing.T, h *harness, s *fakeServer, c cycleCase, before map[string]string) {
	t.Helper()
	expectSame(t, h, s)
	after := s.paths()
	for from, to := range c.moved {
		if after[to] != before[from] {
			t.Errorf("%s: node %s, want %s (was at %s)\n%s", to, after[to], before[from], from, h.dump())
		}
	}
	for p := range after {
		if strings.Contains(p, "перенос") {
			t.Errorf("a temporary name is left: %s", p)
		}
	}
	for p := range localTree(t, h.root) {
		if strings.Contains(p, "перенос") {
			t.Errorf("a temporary name is left on disk: %s", p)
		}
	}
	if res, err := h.e.Pass(context.Background()); err != nil || res.Ops != 0 || res.Deferred != 0 {
		t.Fatalf("extra pass: %+v, %v\n%s", res, err, h.dump())
	}
}

// crashOn makes a harness whose engine stops once, at stage, on the first
// op that matches.
func crashOn(t *testing.T, s *fakeServer, stage string, match func(Op) bool) (*harness, *atomic.Bool) {
	var crashed atomic.Bool
	crashed.Store(true) // armed after the setup
	h := newHarness(t, s, "u1", func(c *Config) {
		c.Hook = func(st string, o Op) error {
			if st == stage && match(o) && crashed.CompareAndSwap(false, true) {
				return errStop
			}
			return nil
		}
	})
	t.Cleanup(h.close)
	return h, &crashed
}

// syncThroughCrash syncs, restarting after the crash.
func syncThroughCrash(t *testing.T, h *harness, crashed *atomic.Bool) {
	t.Helper()
	for range 10 {
		_, err := h.e.SyncOnce(context.Background())
		if err == nil {
			break
		}
		if !errors.Is(err, errStop) {
			t.Fatal(err)
		}
		h.restart()
	}
	if !crashed.Load() {
		t.Fatal("the crash didn't happen")
	}
	h.sync()
}

func isTempOp(o Op) bool { return strings.Contains(o.Path, "перенос") }

func TestCycleLocal(t *testing.T) {
	for _, c := range cycleCases {
		for _, stage := range []string{"", "act", "acted"} {
			t.Run(c.name+"/"+stage, func(t *testing.T) {
				s := newFakeServer()
				h, crashed := crashOn(t, s, stage, func(o Op) bool { return o.Kind == OpMoveRemote && isTempOp(o) })
				for p, v := range c.files {
					s.put(p, v)
				}
				h.selectAll()
				h.sync()
				before := s.paths()
				c.apply(t, h.root)
				if stage == "" {
					h.sync()
				} else {
					crashed.Store(false)
					syncThroughCrash(t, h, crashed)
				}
				checkCycle(t, h, s, c, before)
			})
		}
	}
}

func TestCycleRemote(t *testing.T) {
	for _, c := range cycleCases {
		for _, stage := range []string{"", "act", "acted"} {
			t.Run(c.name+"/"+stage, func(t *testing.T) {
				s := newFakeServer()
				h, crashed := crashOn(t, s, stage, func(o Op) bool { return o.Kind == OpRenameLocal && isTempOp(o) })
				for p, v := range c.files {
					s.put(p, v)
				}
				h.selectAll()
				h.sync()
				before := s.paths()
				c.applyRemote(t, s)
				if stage == "" {
					h.sync()
				} else {
					crashed.Store(false)
					syncThroughCrash(t, h, crashed)
				}
				expectSame(t, h, s)
				if res, err := h.e.Pass(context.Background()); err != nil || res.Ops != 0 || res.Deferred != 0 {
					t.Fatalf("extra pass: %+v, %v\n%s", res, err, h.dump())
				}
				// What was synced locally moved with its node: nothing was
				// downloaded again.
				for from, to := range c.moved {
					if s.paths()[to] != before[from] {
						t.Errorf("%s isn't %s's node", to, from)
					}
				}
				for p := range localTree(t, h.root) {
					if strings.Contains(p, "перенос") {
						t.Errorf("a temporary name is left on disk: %s", p)
					}
				}
			})
		}
	}
}

// The step aside reached the server, then the process died before
// recording it, and the retry fails (the file was edited meanwhile): the
// temporary name on the server is recognised as this device's, and the
// swap completes with the edit.
func TestCycleTempUnrecorded(t *testing.T) {
	s := newFakeServer()
	h, crashed := crashOn(t, s, "acted", func(o Op) bool { return o.Kind == OpMoveRemote && isTempOp(o) })
	s.put("a.txt", "A")
	s.put("b.txt", "B")
	h.selectAll()
	h.sync()
	c := cycleCases[0]
	c.apply(t, h.root)
	crashed.Store(false)
	if _, err := h.e.SyncOnce(context.Background()); !errors.Is(err, errStop) {
		t.Fatalf("sync: %v", err)
	}
	for p := range s.paths() {
		if strings.Contains(p, "перенос") {
			s.put(p, "edited")
		}
	}
	h.restart()
	h.sync()
	expectSame(t, h, s)
	tree := s.tree()
	if len(tree) != 2 || tree["a.txt"] != shaOf("B") || tree["b.txt"] != shaOf("edited") {
		t.Fatalf("server: %v\n%s", tree, h.dump())
	}
}

// A journaled local rename resumed after a crash moves only the entry it
// was planned for: another one put at its path since (ext4 gives a new
// folder a deleted one's inode, so the plan's pairing may not hold) stays.
func TestRenameLocalExpect(t *testing.T) {
	h, _ := fakeHarness(t)
	for _, d := range []string{"a", "other"} {
		if err := os.Mkdir(filepath.Join(h.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st, err := localfs.Stat(h.root, "other")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.e.renameLocal(ctx, Op{Kind: OpRenameLocal, From: "a", Path: "b", Expect: &st}); !errors.Is(err, errReplan) {
		t.Fatalf("rename of another entry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.root, "a")); err != nil {
		t.Fatalf("moved: %v", err)
	}
	// Done before the crash, as it looks: the target is another entry.
	if _, err := h.e.renameLocal(ctx, Op{Kind: OpRenameLocal, From: "gone", Path: "a", Expect: &st}); !errors.Is(err, errReplan) {
		t.Fatalf("another entry taken for the result: %v", err)
	}
	if _, err := h.e.renameLocal(ctx, Op{Kind: OpRenameLocal, From: "other", Path: "b", Expect: &st}); err != nil {
		t.Fatalf("the entry planned: %v", err)
	}
}
