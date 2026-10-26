package engine

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/do-service/cloud/desktop/internal/state"
)

// A sync folder connected with files already in it (a reinstall: a fresh
// state.db, the folder as the old install left it): nothing is uploaded
// or checked by connecting; checked folders pair with what is on disk —
// the same content without a transfer, other content as a conflict copy,
// nothing lost; what is new on disk goes up only inside a checked folder.
func TestAdoptReinstall(t *testing.T) {
	h, s := fakeHarness(t)
	s.put("Docs/a.txt", "a")
	s.put("Docs/b.txt", "b")
	s.put("Photos/p.jpg", "p")
	s.put("top.txt", "t")
	before := s.tree()

	h.write("Docs/a.txt", "a")
	h.write("Docs/b.txt", "b, edited here")
	h.write("Docs/extra.txt", "x")
	h.write("Photos/p.jpg", "p")
	h.write("Mine/m.txt", "m")
	h.write("root.txt", "r")
	ctx := context.Background()
	if err := h.db.Update(ctx, func(tx *state.Tx) error {
		return tx.SetAdopted([]string{"Docs", "Photos", "Mine", "root.txt"})
	}); err != nil {
		t.Fatal(err)
	}
	local := localTree(t, h.root)
	var downloads, uploads int
	s.fault = func(m string) (bool, bool) {
		switch m {
		case "download":
			downloads++
		case "upload":
			uploads++
		}
		return false, false
	}

	h.sync()
	if downloads+uploads != 0 {
		t.Fatalf("connecting transferred: %d down, %d up\n%s", downloads, uploads, h.dump())
	}
	if d := diffTrees(s.tree(), before); d != "" {
		t.Fatalf("the server changed by connecting:\n%s", d)
	}
	if d := diffTrees(localTree(t, h.root), local); d != "" {
		t.Fatalf("the folder changed by connecting:\n%s", d)
	}
	if sel, _ := h.e.Selection(ctx); len(sel) != 0 {
		t.Fatalf("connecting checked %+v", sel)
	}

	// «Отметить папки, которые уже есть на диске»: the cloud's folders of
	// the names the folder held, not Mine (not in the cloud) or top.txt
	// (not on disk).
	ids, err := h.e.Adoptable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	paths := s.paths()
	want := []string{paths["Docs"], paths["Photos"]}
	slices.Sort(ids)
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("adoptable %v, want %v", ids, want)
	}

	// The same content: paired, no transfer.
	if _, err := h.e.Select(ctx, "/Photos"); err != nil {
		t.Fatal(err)
	}
	h.sync()
	if downloads+uploads != 0 {
		t.Fatalf("checking identical Photos transferred: %d down, %d up\n%s", downloads, uploads, h.dump())
	}
	marks, err := h.e.Marks(ctx, []string{paths["Photos"]})
	if err != nil || marks[paths["Photos"]] != (Mark{CheckOn, SyncDone}) {
		t.Fatalf("Photos: %+v, %v", marks, err)
	}

	// Docs: a.txt paired, b.txt edited here kept as a conflict copy (and
	// uploaded) next to the server's, extra.txt uploaded.
	if _, err := h.e.Select(ctx, "/Docs"); err != nil {
		t.Fatal(err)
	}
	h.sync()
	if downloads != 1 || uploads != 2 {
		t.Fatalf("checking Docs: %d down, %d up, want 1 and 2\n%s", downloads, uploads, h.dump())
	}
	if h.read("Docs/a.txt") != "a" || h.read("Docs/b.txt") != "b" || h.read("Docs/extra.txt") != "x" {
		t.Fatal("Docs on disk")
	}
	var copyPath string
	for p, sha := range localTree(t, h.root) {
		if strings.HasPrefix(p, "Docs/b (конфликт") && sha == shaOf("b, edited here") {
			copyPath = p
		}
	}
	if copyPath == "" || !s.has(shaOf("b, edited here")) || !s.has(shaOf("x")) {
		t.Fatalf("the local edit of b.txt is lost: %v\n%s", localTree(t, h.root), h.dump())
	}

	// Still left alone: Mine and root.txt, on disk only.
	tree := s.tree()
	if _, ok := tree["Mine"]; ok {
		t.Fatal("Mine uploaded without being checked")
	}
	if _, ok := tree["root.txt"]; ok {
		t.Fatal("root.txt uploaded without being checked")
	}
	if h.read("Mine/m.txt") != "m" || h.read("root.txt") != "r" {
		t.Fatal("left-alone files changed")
	}

	// Something new in the folder's root is the user's as before:
	// uploaded.
	h.write("new.txt", "n")
	h.sync()
	if _, ok := s.tree()["new.txt"]; !ok || uploads != 3 {
		t.Fatalf("a new file after connecting: %d uploads\n%s", uploads, h.dump())
	}
}
