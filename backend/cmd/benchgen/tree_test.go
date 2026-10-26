package main

import (
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/do-service/cloud/backend/internal/fs"
)

func TestGenTree(t *testing.T) {
	spec := treeSpec{nodes: 20_000, bigDir: 5_000, maxDepth: 6, sampleDirs: 10}
	tr, err := genTree(rand.New(rand.NewPCG(1, 2)), spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.nodes) != spec.nodes {
		t.Fatalf("%d nodes, want %d", len(tr.nodes), spec.nodes)
	}

	children := map[int32]int{}
	keys := map[int32]map[string]bool{}
	sizes := make([]int64, len(tr.nodes))
	files := 0
	for i, n := range tr.nodes {
		if i == 0 {
			if n.parent != -1 || !n.dir || n.name != "" {
				t.Fatalf("root %+v", n)
			}
			continue
		}
		if int(n.parent) >= i {
			t.Fatalf("node %d comes before its parent %d", i, n.parent)
		}
		p := tr.nodes[n.parent]
		if !p.dir {
			t.Fatalf("node %d is under a file", i)
		}
		if n.depth != p.depth+1 || int(n.depth) > spec.maxDepth+1 {
			t.Fatalf("node %d at depth %d, parent at %d", i, n.depth, p.depth)
		}
		if name, key, err := fs.NormalizeName(n.name); err != nil || name != n.name || key != n.key {
			t.Fatalf("node %d name %q key %q: %v", i, n.name, n.key, err)
		}
		if keys[n.parent] == nil {
			keys[n.parent] = map[string]bool{}
		}
		if keys[n.parent][n.key] {
			t.Fatalf("duplicate name_key %q in folder %d", n.key, n.parent)
		}
		keys[n.parent][n.key] = true
		children[n.parent]++
		if !n.dir {
			files++
			if n.mime == "" || n.size < 0 {
				t.Fatalf("file %d: %+v", i, n)
			}
			sizes[i] = n.size
		}
	}
	for i := len(tr.nodes) - 1; i > 0; i-- {
		sizes[tr.nodes[i].parent] += sizes[i]
	}
	for i, n := range tr.nodes {
		if n.dir && n.size != sizes[i] {
			t.Fatalf("folder %d size %d, its subtree holds %d", i, n.size, sizes[i])
		}
	}

	if tr.bigDir == 0 || tr.nodes[tr.bigDir].parent != 0 || children[int32(tr.bigDir)] != spec.bigDir {
		t.Fatalf("big folder %d has %d children, want %d under the root", tr.bigDir, children[int32(tr.bigDir)], spec.bigDir)
	}
	// Mostly files, as on a real instance.
	if share := float64(files) / float64(len(tr.nodes)); share < 0.85 {
		t.Errorf("files are %.0f%% of nodes", share*100)
	}
	if len(tr.sample) != spec.sampleDirs || tr.sample[0] != 0 {
		t.Fatalf("sample %v", tr.sample)
	}
	for _, d := range tr.sample {
		if n := tr.nodes[d]; !n.dir || d == tr.bigDir || (n.parent >= 0 && int(n.parent) == tr.bigDir) {
			t.Fatalf("sampled node %d isn't a regular folder", d)
		}
	}
}

func TestGenTreeDeterministic(t *testing.T) {
	spec := treeSpec{nodes: 3_000, maxDepth: 9, sampleDirs: 5}
	a, err := genTree(rand.New(rand.NewPCG(7, 1)), spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := genTree(rand.New(rand.NewPCG(7, 1)), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed gave different trees")
	}
	c, err := genTree(rand.New(rand.NewPCG(8, 1)), spec)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(a.nodes[1:], c.nodes[1:]) {
		t.Fatal("different seeds gave the same tree")
	}
}

func TestGenTreeTiny(t *testing.T) {
	for n := 1; n < 5; n++ {
		tr, err := genTree(rand.New(rand.NewPCG(1, uint64(n))), treeSpec{nodes: n, maxDepth: 9, sampleDirs: 20})
		if err != nil || len(tr.nodes) != n {
			t.Fatalf("%d nodes: %v, %v", n, tr, err)
		}
	}
	if _, err := genTree(rand.New(rand.NewPCG(1, 1)), treeSpec{nodes: 10, bigDir: 10, maxDepth: 9}); err == nil {
		t.Fatal("a 10-item folder fit into a 10-node tree")
	}
}

func TestSplitNodes(t *testing.T) {
	sizes, err := splitNodes(rand.New(rand.NewPCG(1, 1)), 100, 200_000, 2, 50_000)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for i, s := range sizes {
		total += s
		if s < 1 || (i < 2 && s < 50_002) {
			t.Fatalf("user %d gets %d nodes", i, s)
		}
	}
	if total != 200_000 {
		t.Fatalf("%d nodes in total, want 200000", total)
	}
	if _, err := splitNodes(rand.New(rand.NewPCG(1, 1)), 10, 50_000, 1, 50_000); err == nil {
		t.Fatal("50 000 nodes fit 10 roots and a 50 000-item folder")
	}
}

func TestDedupe(t *testing.T) {
	taken := map[string]bool{"a.txt": true, "a (2).txt": true, "readme": true}
	for in, want := range map[string]string{"b.txt": "b.txt", "a.txt": "a (3).txt", "readme": "readme (2)"} {
		if got := dedupe(in, func(s string) bool { return taken[s] }); got != want {
			t.Errorf("dedupe(%q) = %q, want %q", in, got, want)
		}
	}
}
