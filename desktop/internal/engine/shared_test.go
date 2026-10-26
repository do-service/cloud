package engine

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// Shared folders against the fake server: u2 «Иван» shares folders of
// his with the client's user u1.

// view is what u1's folder should hold: their own tree and, under
// «Доступно мне», the shares named in selected (by their root name).
func (s *fakeServer) view(selected ...string) map[string]string {
	out := s.tree()
	roots, _ := s.Roots(context.Background())
	folder := s.sharedName()
	if len(selected) > 0 {
		out[folder] = "/"
	}
	for _, r := range roots.Roots {
		if r.Kind != "shared" || !contains(selected, r.Name) {
			continue
		}
		maps.Copy(out, s.subtree(r.ID, folder+"/"+r.Name))
	}
	return out
}

// sharedName is «Доступно мне», or «Доступно мне (2)» and on when an own
// folder of the root has the name.
func (s *fakeServer) sharedName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := SharedName
	for i := 2; s.child(s.rootID, localfs.NameKey(name), "") != nil; i++ {
		name = fmt.Sprintf("%s (%d)", SharedName, i)
	}
	return name
}

// viewAll is view with «Доступно мне» checked: every share.
func (s *fakeServer) viewAll() map[string]string {
	roots, _ := s.Roots(context.Background())
	var names []string
	for _, r := range roots.Roots {
		if r.Kind == "shared" {
			names = append(names, r.Name)
		}
	}
	out := s.view(names...)
	out[s.sharedName()] = "/"
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// subtree is the tree below a node, by path from prefix (the node's own
// path, included).
func (s *fakeServer) subtree(id, prefix string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	var walk func(id, p string)
	walk = func(id, p string) {
		n := s.nodes[id]
		if !n.dir {
			out[p] = n.sha
			return
		}
		out[p] = "/"
		for _, c := range s.nodes {
			if c.parent == id && !c.trashed {
				walk(c.id, join(p, c.name))
			}
		}
	}
	walk(id, prefix)
	return out
}

// pathIn is the id of the node at path below top.
func (s *fakeServer) pathIn(top, path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := top
	for _, part := range strings.Split(path, "/") {
		var next string
		for _, c := range s.nodes {
			if c.parent == cur && !c.trashed && c.name == part {
				next = c.id
			}
		}
		if next == "" {
			return ""
		}
		cur = next
	}
	return cur
}

type shareWorld struct {
	t    *testing.T
	h    *harness
	s    *fakeServer
	ivan string // u2's root
}

func newShareWorld(t *testing.T) *shareWorld {
	h, s := fakeHarness(t)
	w := &shareWorld{t: t, h: h, s: s, ivan: s.addOwner("u2", "Иван")}
	s.put("моё.txt", "m")
	s.putIn(w.ivan, "Проект/a.txt", "a1")
	s.putIn(w.ivan, "Проект/sub/b.txt", "b1")
	s.putIn(w.ivan, "Личное/x.txt", "x")
	return w
}

func (w *shareWorld) project() string { return w.s.pathIn(w.ivan, "Проект") }

func (w *shareWorld) expect(selected ...string) {
	w.t.Helper()
	if d := diffTrees(localTree(w.t, w.h.root), w.s.view(selected...)); d != "" {
		w.t.Fatalf("local and server differ:\n%s\n%s", d, w.h.dump())
	}
}

func (w *shareWorld) selectPath(p string) {
	w.t.Helper()
	if _, err := w.h.e.Select(context.Background(), p); err != nil {
		w.t.Fatal(err)
	}
}

func (w *shareWorld) problemKinds() map[string]string {
	out := map[string]string{}
	for _, p := range w.h.problems() {
		out[p.RelPath] = p.Kind
	}
	return out
}

func (w *shareWorld) converged() {
	w.t.Helper()
	if res, err := w.h.e.Pass(context.Background()); err != nil || res.Ops != 0 {
		w.t.Fatalf("extra pass: %+v, %v\n%s", res, err, w.h.dump())
	}
}

const proj = SharedName + "/Проект"

func TestSharedAppearsUnselected(t *testing.T) {
	w := newShareWorld(t)
	w.h.selectAll()
	w.h.sync()
	w.expect()

	// Shared later: listed, not synced until checked — checking "/"
	// doesn't bring it either.
	w.s.share(w.project(), "read", "write")
	w.h.sync()
	w.expect()
	shares, err := Shares(context.Background(), w.h.db)
	if err != nil || len(shares) != 1 || shares[0].Path != "/"+proj || shares[0].Selected || !shares[0].Perms.Has(state.PermWrite) {
		t.Fatalf("shares: %+v, %v", shares, err)
	}

	w.selectPath("/" + proj)
	w.h.sync()
	w.expect("Проект")
	if shares, _ := Shares(context.Background(), w.h.db); !shares[0].Selected {
		t.Fatalf("not selected: %+v", shares)
	}
	sel, err := w.h.e.Selection(context.Background())
	if err != nil || len(sel) != 2 || sel[1].Path != "/"+proj {
		t.Fatalf("selection: %+v, %v", sel, err)
	}

	// Writable: both ways, into the owner's tree.
	w.h.write(proj+"/a.txt", "a2")
	w.h.write(proj+"/new.txt", "n")
	w.s.putIn(w.ivan, "Проект/sub/b.txt", "b2")
	w.h.sync()
	w.expect("Проект")
	if w.h.read(proj+"/sub/b.txt") != "b2" {
		t.Fatal("the owner's edit didn't come")
	}
	if n := w.s.nodes[w.s.pathIn(w.ivan, "Проект/new.txt")]; n == nil || n.owner != "u2" {
		t.Fatal("a new file in the share isn't in the owner's tree")
	}
	// A local delete in a writable share trashes on the server.
	os.Remove(filepath.Join(w.h.root, proj, "new.txt"))
	w.h.sync()
	w.expect("Проект")
	if w.s.pathIn(w.ivan, "Проект/new.txt") != "" {
		t.Fatal("not trashed")
	}
	w.converged()
}

func TestSharedRevokedKeepsEdits(t *testing.T) {
	w := newShareWorld(t)
	w.s.share(w.project(), "read", "write")
	w.h.selectAll()
	w.selectPath("/" + proj)
	w.h.sync()
	w.expect("Проект")

	// Not synced yet when access ends: an edit, a new file, a new folder.
	w.h.write(proj+"/a.txt", "edited")
	w.h.write(proj+"/n.txt", "new")
	w.h.write(proj+"/Новая/c.txt", "c")
	w.s.unshare(w.project())
	w.h.sync()

	if _, err := os.Stat(filepath.Join(w.h.root, proj)); !os.IsNotExist(err) {
		t.Fatalf("the share's folder is still there: %v\n%s", err, w.h.dump())
	}
	// What the user wrote is in their own tree, on the server too.
	own := w.s.tree()
	found := map[string]bool{}
	for p, sha := range own {
		switch {
		case strings.HasPrefix(p, "a (конфликт, Тест, ") && sha == shaOf("edited"):
			found["a"] = true
		case strings.HasPrefix(p, "n (конфликт, Тест, ") && sha == shaOf("new"):
			found["n"] = true
		case strings.HasPrefix(p, "Новая (конфликт, Тест, ") && strings.HasSuffix(p, "/c.txt") && sha == shaOf("c"):
			found["c"] = true
		}
	}
	if len(found) != 3 {
		t.Fatalf("rescued %v of the own tree %v", found, own)
	}
	w.expect()
	kinds := 0
	for _, p := range w.h.problems() {
		if p.Kind == ProblemRescued {
			kinds++
		}
	}
	if kinds != 3 {
		t.Fatalf("problems: %+v", w.h.problems())
	}
	// The owner's files are as they were.
	if got := w.s.subtree(w.project(), "")["a.txt"]; got != shaOf("a1") {
		t.Fatal("the owner's file changed")
	}
	w.converged()

	// Shared again: the selection still holds, it comes back.
	w.s.share(w.project(), "read")
	w.h.sync()
	w.expect("Проект")
	w.converged()
}

func TestSharedDowngrade(t *testing.T) {
	w := newShareWorld(t)
	w.s.share(w.project(), "read", "write")
	w.h.selectAll()
	w.selectPath("/" + proj)
	w.h.sync()

	// Edited while writable, synced after the owner made it read-only:
	// the server's version comes back, the edit is kept as a conflict
	// copy that isn't uploaded.
	w.h.write(proj+"/a.txt", "mine")
	w.h.write(proj+"/n.txt", "new")
	w.s.share(w.project(), "read")
	w.h.sync()
	if w.h.read(proj+"/a.txt") != "a1" {
		t.Fatal("the server's version didn't come back")
	}
	if got := w.s.subtree(w.project(), ""); got["a.txt"] != shaOf("a1") || got["n.txt"] != "" {
		t.Fatalf("uploaded into a read-only share: %v", got)
	}
	kinds := w.problemKinds()
	var copyPath string
	for p, k := range kinds {
		if strings.HasPrefix(p, proj+"/a (конфликт, Тест, ") && k == ProblemReadOnly {
			copyPath = p
		}
	}
	if copyPath == "" || w.h.read(copyPath) != "mine" || kinds[proj+"/n.txt"] != ProblemReadOnly {
		t.Fatalf("problems %v", kinds)
	}
	w.converged()

	// Writable again: what waited goes up.
	w.s.share(w.project(), "read", "write")
	w.h.sync()
	w.expect("Проект")
	if len(w.problemKinds()) != 1 { // the conflict copy's record
		t.Fatalf("problems %v", w.problemKinds())
	}
}

func TestSharedReadOnly(t *testing.T) {
	w := newShareWorld(t)
	w.s.share(w.project(), "read")
	w.h.selectAll()
	w.selectPath("/" + proj)
	w.h.sync()
	w.expect("Проект")
	before := w.s.subtree(w.project(), "")

	// Deleted, renamed, moved out of the share: nothing reaches the
	// owner's tree; the delete and the rename are undone, the move out is
	// a copy.
	os.Remove(filepath.Join(w.h.root, proj, "a.txt"))
	if err := os.Rename(filepath.Join(w.h.root, proj, "sub"), filepath.Join(w.h.root, proj, "sub2")); err != nil {
		t.Fatal(err)
	}
	w.h.sync()
	w.expect("Проект")
	if err := os.Rename(filepath.Join(w.h.root, proj, "sub", "b.txt"), filepath.Join(w.h.root, "b-copy.txt")); err != nil {
		t.Fatal(err)
	}
	// Own file moved into the share: the move is undone.
	if err := os.Rename(filepath.Join(w.h.root, "моё.txt"), filepath.Join(w.h.root, proj, "моё.txt")); err != nil {
		t.Fatal(err)
	}
	// Written into «Доступно мне» itself: a problem, not an upload.
	w.h.write(SharedName+"/сюда.txt", "s")
	w.h.sync()
	if w.s.tree()["b-copy.txt"] != shaOf("b1") {
		t.Fatalf("the copy out of the share isn't in the own tree: %v", w.s.tree())
	}
	if d := diffTrees(w.s.subtree(w.project(), ""), before); d != "" {
		t.Fatalf("the owner's tree changed:\n%s", d)
	}
	if k := w.problemKinds()[SharedName+"/сюда.txt"]; k != ProblemReadOnly {
		t.Fatalf("problems %v", w.problemKinds())
	}
	os.Remove(filepath.Join(w.h.root, SharedName, "сюда.txt"))
	w.h.sync()
	w.expect("Проект")
	w.converged()

	// Deleting the share's folder (or «Доступно мне») doesn't empty it:
	// it comes back; unchecking is how it leaves the disk.
	os.RemoveAll(filepath.Join(w.h.root, SharedName))
	w.h.sync()
	w.expect("Проект")
	if err := w.h.e.Unselect(context.Background(), "/"+proj); err != nil {
		t.Fatal(err)
	}
	w.h.sync()
	w.expect()
}

func TestSharedWritableMoveOut(t *testing.T) {
	w := newShareWorld(t)
	w.s.share(w.project(), "read", "write")
	w.h.selectAll()
	w.selectPath("/" + proj)
	w.h.sync()

	// Between owners nothing moves on the server: a copy and a delete.
	if err := os.Rename(filepath.Join(w.h.root, proj, "sub"), filepath.Join(w.h.root, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(w.h.root, "моё.txt"), filepath.Join(w.h.root, proj, "моё.txt")); err != nil {
		t.Fatal(err)
	}
	w.h.sync()
	w.expect("Проект")
	own, theirs := w.s.tree(), w.s.subtree(w.project(), "")
	if own["sub/b.txt"] != shaOf("b1") || theirs["sub"] != "" || theirs["моё.txt"] != shaOf("m") || own["моё.txt"] != "" {
		t.Fatalf("own %v, shared %v", own, theirs)
	}
	w.converged()
}

func TestSharedNameClash(t *testing.T) {
	w := newShareWorld(t)
	petr := w.s.addOwner("u3", "Пётр")
	w.s.putIn(petr, "Проект/p.txt", "p")
	w.s.share(w.project(), "read")
	// An own folder takes «Доступно мне»: the shares go to «(2)».
	w.s.put(SharedName+"/своё.txt", "own")
	w.h.selectAll()
	w.selectPath("/" + SharedName + " (2)/Проект")
	w.h.sync()
	if w.h.read(SharedName+" (2)/Проект/a.txt") != "a1" || w.h.read(SharedName+"/своё.txt") != "own" {
		t.Fatalf("local: %v", localTree(t, w.h.root))
	}

	// A second «Проект» from someone else: both by their owners.
	w.s.share(w.s.pathIn(petr, "Проект"), "read")
	w.h.sync()
	if _, err := w.h.e.Select(context.Background(), "/"+SharedName+" (2)/Проект (Пётр)"); err != nil {
		t.Fatal(err)
	}
	w.h.sync()
	got := localTree(t, w.h.root)
	if got[SharedName+" (2)/Проект (Иван)/a.txt"] != shaOf("a1") || got[SharedName+" (2)/Проект (Пётр)/p.txt"] != shaOf("p") ||
		got[SharedName+" (2)/Проект"] != "" {
		t.Fatalf("local: %v\n%s", got, w.h.dump())
	}
	w.converged()
}

func TestSharedQuota(t *testing.T) {
	w := newShareWorld(t)
	w.s.share(w.project(), "read", "write")
	w.h.selectAll()
	w.selectPath("/" + proj)
	w.h.sync()
	// The owner's quota, not the user's.
	w.s.mu.Lock()
	w.s.quota["u2"] = 10
	w.s.mu.Unlock()
	w.h.write(proj+"/big.txt", "more than ten bytes")
	w.h.write("моё-большое.txt", "more than ten bytes")
	w.h.sync()
	if k := w.problemKinds()[proj+"/big.txt"]; k != ProblemQuota {
		t.Fatalf("problems %v", w.problemKinds())
	}
	if w.s.tree()["моё-большое.txt"] == "" {
		t.Fatal("the user's own upload was held by the owner's quota")
	}
}

func TestSharedTrashedByOwner(t *testing.T) {
	w := newShareWorld(t)
	w.s.share(w.project(), "read")
	w.h.selectAll()
	w.selectPath("/" + proj)
	w.h.sync()
	w.h.write(proj+"/n.txt", "new")
	// Trashed, not unshared: a tombstone, no rootsChanged.
	id := w.project()
	w.s.mu.Lock()
	err := w.s.trash(id, "")
	w.s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	w.h.sync()
	if _, err := os.Stat(filepath.Join(w.h.root, proj)); !os.IsNotExist(err) {
		t.Fatalf("still there: %v\n%s", err, w.h.dump())
	}
	found := false
	for p, sha := range w.s.tree() {
		found = found || strings.HasPrefix(p, "n (конфликт, Тест, ") && sha == shaOf("new")
	}
	if !found {
		t.Fatalf("the new file wasn't kept: %v", w.s.tree())
	}
	w.expect()
	w.converged()
}
