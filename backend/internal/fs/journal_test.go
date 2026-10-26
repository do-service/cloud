package fs

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/do-service/cloud/backend/internal/access"
)

type journalRow struct {
	Seq    int64
	Node   string
	Op     journalOp
	Scopes []string
	Device string
}

// journalAfter returns ownerID's journal rows after seq, in seq order.
func (e env) journalAfter(t *testing.T, ownerID string, seq int64) []journalRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT seq, node_id::text, op, coalesce(scopes::text[], '{}'), coalesce(device_id::text, '')
		FROM sync_journal WHERE owner_id = $1 AND seq > $2 ORDER BY seq`, ownerID, seq)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []journalRow
	for rows.Next() {
		var r journalRow
		if err := rows.Scan(&r.Seq, &r.Node, &r.Op, &r.Scopes, &r.Device); err != nil {
			t.Fatal(err)
		}
		slices.Sort(r.Scopes)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e env) treeSeq(t *testing.T, ownerID string) int64 {
	t.Helper()
	var seq int64
	err := e.pool.QueryRow(context.Background(), `SELECT coalesce((SELECT tree_seq FROM sync_heads WHERE user_id = $1), 0)`, ownerID).Scan(&seq)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// uploadLink makes a public upload link on nodeID and returns its principal.
func (e env) uploadLink(t *testing.T, nodeID string) string {
	t.Helper()
	hash := make([]byte, 32)
	_, _ = crand.Read(hash)
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO links (node_id, token_hash, token_enc, mode) VALUES ($1, $2, '\x00', 'upload') RETURNING id`,
		nodeID, hash).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return access.LinkPrincipal(id)
}

type wantRow struct {
	op     journalOp
	node   string
	scopes []string
}

// Every write method journals what it changed: one row per node it
// creates, replaces, moves, trashes or restores, with the shared nodes
// on its chains as scopes, at the next seq of the owner's tree.
func TestJournalWrites(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	bob, carol := e.newUser(t, "bob"), e.newUser(t, "carol")
	if _, err := e.svc.EnsureRoot(ctx, bob); err != nil {
		t.Fatal(err)
	}
	docs := e.mkdir(t, e.root.ID, "Docs")
	shared := e.mkdir(t, e.root.ID, "Shared")
	sub := e.mkdir(t, shared.ID, "Sub")
	e.share(t, shared.ID, bob, access.Read|access.Write)
	e.share(t, sub.ID, bob, access.Read)
	both := []string{shared.ID, sub.ID}
	slices.Sort(both)
	plain := e.file(t, docs.ID, "plain.txt", "v1")
	inShared := e.file(t, shared.ID, "in.txt", "v1")

	must := func(n Node, err error) Node {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	put := func(actor, parent, name, content string) Node {
		t.Helper()
		n, _, err := e.svc.PutFile(ctx, actor, parent, name, strings.NewReader(content), int64(len(content)), PutCond{})
		return must(n, err)
	}
	var trashed Node
	cases := []struct {
		name string
		do   func() []wantRow
	}{
		{"CreateDir", func() []wantRow {
			return []wantRow{{opCreate, e.mkdir(t, docs.ID, "New").ID, nil}}
		}},
		{"CreateDir in a share", func() []wantRow {
			return []wantRow{{opCreate, e.mkdir(t, sub.ID, "Deep").ID, both}}
		}},
		{"CreateDir by the recipient", func() []wantRow {
			return []wantRow{{opCreate, must(e.svc.CreateDir(ctx, bob, shared.ID, "Bob's")).ID, []string{shared.ID}}}
		}},
		{"CreateFile", func() []wantRow {
			return []wantRow{{opCreate, e.file(t, docs.ID, "a.txt", "a").ID, nil}}
		}},
		{"upload", func() []wantRow {
			return []wantRow{{opCreate, e.upload(t, docs.ID, "up.txt", "u", ConflictFail).ID, nil}}
		}},
		{"upload replacing", func() []wantRow {
			n := e.upload(t, shared.ID, "in.txt", "v2", ConflictReplace)
			if n.ID != inShared.ID {
				t.Fatalf("replaced %s, want %s", n.ID, inShared.ID)
			}
			return []wantRow{{opContent, inShared.ID, []string{shared.ID}}}
		}},
		{"upload by node id (sync client)", func() []wantRow {
			up, err := e.svc.ReserveUpload(ctx, bob, Upload{NodeID: inShared.ID, IfMatch: e.get(t, inShared.ID).ETag, Size: 2})
			if err != nil {
				t.Fatal(err)
			}
			must(e.svc.FinishUpload(ctx, bob, up.ID, e.stage(t, up.ID, "v4")))
			return []wantRow{{opContent, inShared.ID, []string{shared.ID}}}
		}},
		{"upload renaming", func() []wantRow {
			return []wantRow{{opCreate, e.upload(t, docs.ID, "plain.txt", "p", ConflictRename).ID, nil}}
		}},
		{"upload through a link", func() []wantRow {
			link := e.uploadLink(t, docs.ID)
			up, err := e.svc.ReserveUpload(ctx, link, Upload{ParentID: docs.ID, Name: "drop.txt", Size: 1, OnConflict: ConflictFail})
			if err != nil {
				t.Fatal(err)
			}
			return []wantRow{{opCreate, must(e.svc.FinishUpload(ctx, link, up.ID, e.stage(t, up.ID, "d"))).ID, nil}}
		}},
		{"PutFile new (WebDAV PUT)", func() []wantRow {
			return []wantRow{{opCreate, put(e.user, docs.ID, "put.txt", "p").ID, nil}}
		}},
		{"PutFile replacing (WebDAV PUT)", func() []wantRow {
			put(e.user, docs.ID, "plain.txt", "v2")
			return []wantRow{{opContent, plain.ID, nil}}
		}},
		{"WriteFile (WOPI)", func() []wantRow {
			must(e.svc.WriteFile(ctx, bob, inShared.ID, strings.NewReader("v3"), 2, PutCond{}))
			return []wantRow{{opContent, inShared.ID, []string{shared.ID}}}
		}},
		{"RestoreVersion", func() []wantRow {
			versions, _, err := e.svc.ListVersions(ctx, e.user, plain.ID)
			if err != nil || len(versions) == 0 {
				t.Fatalf("versions: %v, %v", versions, err)
			}
			must(e.svc.RestoreVersion(ctx, e.user, versions[0].ID))
			return []wantRow{{opContent, plain.ID, nil}}
		}},
		{"Rename", func() []wantRow {
			must(e.svc.Rename(ctx, e.user, plain.ID, "renamed.txt"))
			return []wantRow{{opMove, plain.ID, nil}}
		}},
		{"Rename to the same name", func() []wantRow {
			must(e.svc.Rename(ctx, e.user, plain.ID, "renamed.txt"))
			return nil
		}},
		{"Rename a shared folder", func() []wantRow {
			must(e.svc.Rename(ctx, e.user, sub.ID, "Sub2"))
			return []wantRow{{opMove, sub.ID, both}}
		}},
		{"Move into a share (WebDAV MOVE)", func() []wantRow {
			must(e.svc.Move(ctx, e.user, plain.ID, sub.ID, ""))
			return []wantRow{{opMove, plain.ID, both}}
		}},
		{"Move out of a share", func() []wantRow {
			must(e.svc.Move(ctx, e.user, plain.ID, docs.ID, ""))
			return []wantRow{{opMove, plain.ID, both}}
		}},
		{"Move a folder: one row", func() []wantRow {
			must(e.svc.Move(ctx, e.user, docs.ID, shared.ID, ""))
			return []wantRow{{opMove, docs.ID, []string{shared.ID}}}
		}},
		{"MOVE over an existing file (WebDAV: trash, then move)", func() []wantRow {
			target := e.file(t, e.root.ID, "target.txt", "t")
			src := e.file(t, e.root.ID, "src.txt", "s")
			e.trash(t, target.ID)
			must(e.svc.Move(ctx, e.user, src.ID, e.root.ID, "target.txt"))
			return []wantRow{{opCreate, target.ID, nil}, {opCreate, src.ID, nil}, {opTrash, target.ID, nil}, {opMove, src.ID, nil}}
		}},
		{"Trash a folder (WebDAV DELETE): one row", func() []wantRow {
			e.trash(t, docs.ID)
			trashed = docs
			return []wantRow{{opTrash, docs.ID, []string{shared.ID}}}
		}},
		{"Restore", func() []wantRow {
			must(e.svc.Restore(ctx, e.user, trashed.ID))
			return []wantRow{{opRestore, trashed.ID, []string{shared.ID}}}
		}},
		{"Purge: no row", func() []wantRow {
			e.trash(t, trashed.ID)
			if err := e.svc.Purge(ctx, e.user, trashed.ID); err != nil {
				t.Fatal(err)
			}
			return []wantRow{{opTrash, trashed.ID, []string{shared.ID}}}
		}},
		{"Copy a folder: a row per node", func() []wantRow {
			src := e.mkdir(t, e.root.ID, "Src")
			f := e.file(t, src.ID, "f.txt", "f")
			c := must(e.svc.Copy(ctx, e.user, src.ID, sub.ID, "", CopyOptions{}))
			copied := must(liveChild(ctx, e.pool, c.ID, "f.txt"))
			return []wantRow{{opCreate, src.ID, nil}, {opCreate, f.ID, nil}, {opCreate, c.ID, both}, {opCreate, copied.ID, both}}
		}},
		{"Copy a file (WebDAV COPY)", func() []wantRow {
			return []wantRow{{opCreate, must(e.svc.Copy(ctx, e.user, inShared.ID, e.root.ID, "copy.txt", CopyOptions{})).ID, nil}}
		}},
		{"refused write: no row", func() []wantRow {
			if _, err := e.svc.CreateDir(ctx, e.user, e.root.ID, "Shared"); !errors.Is(err, ErrNameConflict) {
				t.Fatalf("conflict: %v", err)
			}
			if _, err := e.svc.CreateDir(ctx, carol, sub.ID, "x"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("no access: %v", err)
			}
			return nil
		}},
	}
	for _, tc := range cases {
		before := e.treeSeq(t, e.user)
		want := tc.do()
		got := e.journalAfter(t, e.user, before)
		if len(got) != len(want) {
			t.Fatalf("%s: rows %+v, want %+v", tc.name, got, want)
		}
		for i, w := range want {
			g := got[i]
			if g.Seq != before+int64(i)+1 || g.Op != w.op || g.Node != w.node || !slices.Equal(g.Scopes, w.scopes) {
				t.Errorf("%s: row %d is %+v, want seq %d %+v", tc.name, i, g, before+int64(i)+1, w)
			}
		}
		if seq := e.treeSeq(t, e.user); seq != before+int64(len(want)) {
			t.Errorf("%s: head %d, want %d", tc.name, seq, before+int64(len(want)))
		}
	}
	// Writes in someone's tree journal there, whoever makes them.
	if rows := e.journalAfter(t, bob, 0); len(rows) != 0 {
		t.Errorf("bob's journal: %+v", rows)
	}
}

func TestJournalDevice(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	const device = "6f1c2a3e-8a4b-4c5d-9e6f-7a8b9c0d1e2f"
	n, err := e.svc.CreateDir(WithDevice(ctx, device), e.user, e.root.ID, "From device")
	if err != nil {
		t.Fatal(err)
	}
	e.mkdir(t, e.root.ID, "From the web")
	rows := e.journalAfter(t, e.user, 0)
	if len(rows) != 2 || rows[0].Node != n.ID || rows[0].Device != device || rows[1].Device != "" {
		t.Fatalf("rows %+v", rows)
	}
}

// nodeState is what a sync client mirrors of a node. A folder's ETag,
// size and mtime aren't in it: they follow from what is below it.
type nodeState struct {
	Parent, Name string
	Content      string // a file's ETag
	Trashed      bool
}

func (e env) snapshot(t *testing.T) map[string]nodeState {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT id::text, coalesce(parent_id::text, ''), name, CASE WHEN kind = 'file' THEN etag ELSE '' END, trashed_at IS NOT NULL
		FROM nodes WHERE owner_id = $1`, e.user)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	snap := map[string]nodeState{}
	for rows.Next() {
		var id string
		var s nodeState
		if err := rows.Scan(&id, &s.Parent, &s.Name, &s.Content, &s.Trashed); err != nil {
			t.Fatal(err)
		}
		snap[id] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snap
}

// Replaying the journal from any point finds every change since: a node
// whose place, name or content differs from a snapshot taken at that seq
// has a row of its own, and one trashed, restored or purged since has a
// row of its own or of a folder above it, then or now.
func TestJournalReplay(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	rnd := rand.New(rand.NewPCG(11, 1))
	pick := func(ids []string) string { return ids[rnd.IntN(len(ids))] }
	ids := func(q string) []string {
		rows, err := e.pool.Query(ctx, q, e.user)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	liveDirs := func() []string {
		return ids(`SELECT id::text FROM nodes WHERE owner_id = $1 AND kind = 'dir' AND trashed_at IS NULL ORDER BY created_at, id`)
	}
	liveFiles := func() []string {
		return ids(`SELECT id::text FROM nodes WHERE owner_id = $1 AND kind = 'file' AND trashed_at IS NULL ORDER BY created_at, id`)
	}
	liveNonRoot := func() []string {
		return ids(`SELECT id::text FROM nodes WHERE owner_id = $1 AND parent_id IS NOT NULL AND trashed_at IS NULL ORDER BY created_at, id`)
	}
	trashItems := func() []string {
		return ids(`SELECT id::text FROM nodes WHERE owner_id = $1 AND trash_root ORDER BY trashed_at, id`)
	}
	name := func() string { return fmt.Sprintf("n%d", rnd.IntN(12)) }
	content := func() string { return fmt.Sprintf("c%d", rnd.IntN(1000)) }

	ops := []func() error{
		func() error { _, err := e.svc.CreateDir(ctx, e.user, pick(liveDirs()), name()); return err },
		func() error {
			c := content()
			_, err := e.svc.CreateFile(ctx, e.user, pick(liveDirs()), name(), strings.NewReader(c), int64(len(c)))
			return err
		},
		func() error {
			c := content()
			_, _, err := e.svc.PutFile(ctx, e.user, pick(liveDirs()), name(), strings.NewReader(c), int64(len(c)), PutCond{})
			return err
		},
		func() error {
			files := liveFiles()
			if len(files) == 0 {
				return nil
			}
			c := content()
			_, err := e.svc.WriteFile(ctx, e.user, pick(files), strings.NewReader(c), int64(len(c)), PutCond{})
			return err
		},
		func() error {
			files := liveFiles()
			if len(files) == 0 {
				return nil
			}
			versions, _, err := e.svc.ListVersions(ctx, e.user, pick(files))
			if err != nil || len(versions) == 0 {
				return err
			}
			_, err = e.svc.RestoreVersion(ctx, e.user, versions[rnd.IntN(len(versions))].ID)
			return err
		},
		func() error {
			nodes := liveNonRoot()
			if len(nodes) == 0 {
				return nil
			}
			_, err := e.svc.Move(ctx, e.user, pick(nodes), pick(liveDirs()), "")
			return err
		},
		func() error {
			nodes := liveNonRoot()
			if len(nodes) == 0 {
				return nil
			}
			_, err := e.svc.Rename(ctx, e.user, pick(nodes), name())
			return err
		},
		func() error {
			nodes := liveNonRoot()
			if len(nodes) == 0 {
				return nil
			}
			return e.svc.Trash(ctx, e.user, pick(nodes))
		},
		func() error {
			items := trashItems()
			if len(items) == 0 {
				return nil
			}
			_, err := e.svc.Restore(ctx, e.user, pick(items))
			return err
		},
		func() error {
			items := trashItems()
			if len(items) == 0 {
				return nil
			}
			return e.svc.Purge(ctx, e.user, pick(items))
		},
		func() error {
			nodes := liveNonRoot()
			if len(nodes) == 0 {
				return nil
			}
			_, err := e.svc.Copy(ctx, e.user, pick(nodes), pick(liveDirs()), "", CopyOptions{Rename: true})
			return err
		},
	}
	// Weights: creating more often than removing, so the tree grows.
	weights := []int{4, 4, 3, 2, 1, 3, 2, 2, 2, 1, 1}
	var total int
	for _, w := range weights {
		total += w
	}
	expected := []error{ErrNameConflict, ErrCycle, ErrNotFound, ErrIsRoot}

	type checkpoint struct {
		seq  int64
		snap map[string]nodeState
	}
	points := []checkpoint{{e.treeSeq(t, e.user), e.snapshot(t)}}
	for i := range 400 {
		r := rnd.IntN(total)
		op := 0
		for ; r >= weights[op]; op++ {
			r -= weights[op]
		}
		if err := ops[op](); err != nil && !slices.ContainsFunc(expected, func(want error) bool { return errors.Is(err, want) }) {
			t.Fatalf("step %d, op %d: %v", i, op, err)
		}
		points = append(points, checkpoint{e.treeSeq(t, e.user), e.snapshot(t)})
	}

	journal := e.journalAfter(t, e.user, 0)
	for i, r := range journal {
		if r.Seq != int64(i)+1 {
			t.Fatalf("seq %d at position %d: not contiguous", r.Seq, i)
		}
	}
	end := points[len(points)-1].snap
	if len(end) < 50 || len(journal) < 200 {
		t.Fatalf("too little happened to test: %d nodes, %d rows", len(end), len(journal))
	}
	// above lists id and its folders up to the root, as snap has them.
	above := func(snap map[string]nodeState, id string) []string {
		var out []string
		for id != "" {
			out = append(out, id)
			s, ok := snap[id]
			if !ok {
				break
			}
			id = s.Parent
		}
		return out
	}
	for _, p := range points {
		changed := map[string]bool{}
		for _, r := range journal[p.seq:] {
			changed[r.Node] = true
		}
		// A folder's row stands for its subtree: a node trashed, restored
		// or purged with it, or after it moved, needs no row of its own.
		covered := func(id string) bool {
			return slices.ContainsFunc(append(above(p.snap, id), above(end, id)...), func(a string) bool { return changed[a] })
		}
		for id, now := range end {
			then, existed := p.snap[id]
			switch {
			case !existed:
				if !changed[id] {
					t.Errorf("seq %d: %s created since, not journaled", p.seq, id)
				}
			case then.Parent != now.Parent || then.Name != now.Name || then.Content != now.Content:
				if !changed[id] {
					t.Errorf("seq %d: %s changed %+v -> %+v, not journaled", p.seq, id, then, now)
				}
			case then.Trashed != now.Trashed:
				if !covered(id) {
					t.Errorf("seq %d: %s trashed %v -> %v, not journaled", p.seq, id, then.Trashed, now.Trashed)
				}
			}
		}
		for id, then := range p.snap {
			if _, ok := end[id]; !ok && !then.Trashed && !covered(id) {
				t.Errorf("seq %d: %s purged since, its trashing not journaled", p.seq, id)
			}
		}
	}
}
