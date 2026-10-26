package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"pgregory.net/rapid"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// TestSyncProperty is the stateful property test of docs/desktop-client.md
// («Тесты»): random edits on both sides (the fake server and a real temp
// folder), syncs that hit network errors or crash in the middle, cursor
// expiry, an unplugged disk. Few names, so renames collide: swaps and
// longer cycles of renames on either side, and folders moved into what
// takes their place. Invariants:
//
//   - after every pass, content that disappeared from the disk is on the
//     server (live, in versions or trash): the engine never destroys a
//     local edit it hadn't uploaded;
//   - with the marker gone a pass changes nothing on either side;
//   - after quiescence the folder and the server have the same tree
//     (names by key, content by SHA-256), in a bounded number of passes,
//     and one more pass does nothing (no ping-pong).
//
// A second owner shares two folders with the user, one read-only, one
// writable, both checked (with «Доступно мне»): they edit and move in
// them, revoke and share again, make the writable one read-only and
// back. The same invariants hold with «Доступно мне» as the shares' tree
// after quiescence, but for what the user wrote where they can't write:
// that stays on the disk, a problem (readonly), and isn't compared.
//
// go test ./internal/engine -run SyncProperty -rapid.checks=1000 -rapid.seed=N
//
// On Linux a seed may not replay: ext4 hands a freed inode to the next
// new file, so which entries share a file id depends on the whole disk.
// Delete testdata/rapid first: rapid replays its fail files before
// anything else.
func TestSyncProperty(t *testing.T) {
	foldCase, foldForm := diskFolds(t)
	rapid.Check(t, func(rt *rapid.T) {
		dir, err := os.MkdirTemp(t.TempDir(), "prop-")
		if err != nil {
			rt.Fatal(err)
		}
		m := newPropModel(rt, dir, foldCase, foldForm)
		defer m.h.close()
		rt.Repeat(map[string]func(*rapid.T){
			"localWrite":  m.localWrite,
			"localMkdir":  m.localMkdir,
			"localDelete": m.localDelete,
			"localMove":   m.localMove,
			"remoteWrite": m.remoteWrite,
			"remoteMkdir": m.remoteMkdir,
			"remoteTrash": m.remoteTrash,
			"remoteMove":  m.remoteMove,
			"ownerWrite":  m.ownerWrite,
			"ownerTrash":  m.ownerTrash,
			"ownerMove":   m.ownerMove,
			"reshare":     m.reshare,
			"flipRights":  m.flipRights,
			"sync":        m.sync,
			"restart":     func(*rapid.T) { m.h.restart() },
			"expire":      func(*rapid.T) { m.s.expire() },
			"loseMarker":  m.loseMarker,
		})
		m.quiesce(rt)
	})
}

type propModel struct {
	s        *fakeServer
	h        *harness
	foldCase bool
	foldForm bool
	n        int
	// u2's root and the folders it shares: ro read-only, rw writable
	// (while rwWrite).
	ivan, ro, rw string
	rwWrite      bool
	// faults: the fake server's failures and the crash hook, decided by a
	// seeded generator (calls come from the engine, not the test).
	mu     sync.Mutex
	rng    *rand.Rand
	pFault float64
	crashK atomic.Int64
}

func newPropModel(rt *rapid.T, dir string, foldCase, foldForm bool) *propModel {
	m := &propModel{s: newFakeServer(), foldCase: foldCase, foldForm: foldForm}
	m.rng = rand.New(rand.NewPCG(rapid.Uint64().Draw(rt, "seed"), 1))
	m.s.fault = func(string) (bool, bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.pFault == 0 || m.rng.Float64() >= m.pFault {
			return false, false
		}
		return m.rng.IntN(2) == 0, true
	}
	m.h = newHarnessIn(rt, dir, m.s, "u1", func(c *Config) {
		// One transfer at a time keeps a run reproducible from its seed.
		c.LargeTransfers, c.SmallTransfers = 1, 1
		c.ConfirmDeletes = true
		c.Hook = func(string, Op) error {
			if k := m.crashK.Add(-1); k == 0 {
				return errStop
			}
			return nil
		}
	})
	m.ivan = m.s.addOwner("u2", "Иван")
	m.s.putIn(m.ivan, "Общая/r.txt", "r")
	m.s.putIn(m.ivan, "Совместная/w.txt", "w")
	m.s.putIn(m.ivan, "Личное/p.txt", "p")
	m.ro, m.rw, m.rwWrite = m.s.pathIn(m.ivan, "Общая"), m.s.pathIn(m.ivan, "Совместная"), true
	m.s.share(m.ro, "read")
	m.s.share(m.rw, "read", "write")
	m.h.selectAll()
	if _, err := m.h.e.Select(context.Background(), "/"+SharedName); err != nil {
		rt.Fatal(err)
	}
	return m
}

// diskFolds tells whether the temp dir's disk takes names differing in
// case, and in Unicode form (NFC, NFD), for one: APFS folds both, NTFS
// only case, ext4 neither.
func diskFolds(t *testing.T) (foldCase, foldForm bool) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "Xé"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, errCase := os.Stat(filepath.Join(d, "xé"))
	_, errForm := os.Stat(filepath.Join(d, "Xé"))
	return errCase == nil, errForm == nil
}

// Names: few, so they collide; case and Unicode variants only where the
// disk folds them (otherwise they're two files the server can't hold, a
// problem by design).
func (m *propModel) names() []string {
	switch {
	case m.foldCase && m.foldForm:
		return []string{"a.txt", "A.TXT", "b", "é.txt", "é.txt", "Dir"}
	case m.foldCase:
		return []string{"a.txt", "A.TXT", "b", "é.txt", "Dir"}
	}
	return []string{"a.txt", "b", "é.txt", "dir"}
}

// drawPath is a path in the own tree, a share's or «Доступно мне».
func (m *propModel) drawPath(rt *rapid.T, label string) string {
	names := m.names()
	depth := rapid.IntRange(1, 3).Draw(rt, label+"Depth")
	parts := make([]string, depth)
	for i := range parts {
		parts[i] = rapid.SampledFrom(names).Draw(rt, label)
	}
	prefix := rapid.SampledFrom([]string{"", "", "", SharedName + "/Общая/", SharedName + "/Совместная/", SharedName + "/"}).
		Draw(rt, label+"Tree")
	return prefix + strings.Join(parts, "/")
}

// drawIn is a path below u2's folder top.
func (m *propModel) drawIn(rt *rapid.T, label string) string {
	names := m.names()
	parts := make([]string, rapid.IntRange(1, 2).Draw(rt, label+"Depth"))
	for i := range parts {
		parts[i] = rapid.SampledFrom(names).Draw(rt, label)
	}
	return strings.Join(parts, "/")
}

func (m *propModel) ownerWrite(rt *rapid.T) {
	top := rapid.SampledFrom([]string{m.ro, m.rw}).Draw(rt, "share")
	m.s.putIn(top, m.drawIn(rt, "path"), m.content())
}

// ivanIDs: u2's nodes but the shared folders themselves (moving or
// trashing those is what reshare does to the user).
func (m *propModel) ivanIDs() []string {
	var out []string
	for _, id := range m.s.liveIn("u2") {
		if id != m.ro && id != m.rw {
			out = append(out, id)
		}
	}
	return out
}

func (m *propModel) ownerTrash(rt *rapid.T) {
	ids := m.ivanIDs()
	if len(ids) == 0 {
		return
	}
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	_ = m.s.trash(rapid.SampledFrom(ids).Draw(rt, "victim"), "")
}

// ownerMove moves within u2's tree: between the shares, out of them and
// into them from his private folder.
func (m *propModel) ownerMove(rt *rapid.T) {
	ids := m.ivanIDs()
	if len(ids) == 0 {
		return
	}
	id := rapid.SampledFrom(ids).Draw(rt, "node")
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	dirs := []string{m.ivan, m.ro, m.rw}
	for _, d := range ids {
		if n := m.s.nodes[d]; n != nil && n.dir {
			dirs = append(dirs, d)
		}
	}
	_, _ = m.s.move(id, rapid.SampledFrom(dirs).Draw(rt, "into"), rapid.SampledFrom(m.names()).Draw(rt, "name"), "")
}

// reshare revokes a share, or shares it again.
func (m *propModel) reshare(rt *rapid.T) {
	id := rapid.SampledFrom([]string{m.ro, m.rw}).Draw(rt, "share")
	m.s.mu.Lock()
	_, on := m.s.shares[id]
	m.s.mu.Unlock()
	switch {
	case on:
		m.s.unshare(id)
	case id == m.ro:
		m.s.share(id, "read")
	case m.rwWrite:
		m.s.share(id, "read", "write")
	default:
		m.s.share(id, "read")
	}
}

// flipRights makes the writable share read-only, or writable again.
func (m *propModel) flipRights(rt *rapid.T) {
	m.rwWrite = !m.rwWrite
	m.s.mu.Lock()
	_, on := m.s.shares[m.rw]
	m.s.mu.Unlock()
	if !on {
		return
	}
	if m.rwWrite {
		m.s.share(m.rw, "read", "write")
	} else {
		m.s.share(m.rw, "read")
	}
}

func (m *propModel) content() string {
	m.n++
	return fmt.Sprintf("content-%d", m.n)
}

func (m *propModel) abs(rel string) string { return filepath.Join(m.h.root, filepath.FromSlash(rel)) }

// localEntries lists what's on disk, ignoring the client's own files.
func (m *propModel) localEntries() []string {
	var out []string
	for p := range localTree(m.h.t, m.h.root) {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (m *propModel) localWrite(rt *rapid.T) {
	rel := m.drawPath(rt, "path")
	if entries := m.localEntries(); len(entries) > 0 && rapid.Bool().Draw(rt, "overwrite") {
		rel = rapid.SampledFrom(entries).Draw(rt, "existing")
	}
	if fi, err := os.Stat(m.abs(rel)); err == nil && fi.IsDir() {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.abs(rel)), 0o755); err != nil {
		return // a file is in the way
	}
	_ = os.WriteFile(m.abs(rel), []byte(m.content()), 0o644)
}

func (m *propModel) localMkdir(rt *rapid.T) {
	_ = os.MkdirAll(m.abs(m.drawPath(rt, "dir")), 0o755)
}

func (m *propModel) localDelete(rt *rapid.T) {
	entries := m.localEntries()
	if len(entries) == 0 {
		return
	}
	_ = os.RemoveAll(m.abs(rapid.SampledFrom(entries).Draw(rt, "victim")))
}

func (m *propModel) localMove(rt *rapid.T) {
	entries := m.localEntries()
	if len(entries) == 0 {
		return
	}
	from := rapid.SampledFrom(entries).Draw(rt, "from")
	to := m.drawPath(rt, "to")
	if under(to, from) {
		return
	}
	if _, err := os.Lstat(m.abs(to)); err == nil && !strings.EqualFold(to, from) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.abs(to)), 0o755); err != nil {
		return
	}
	_ = os.Rename(m.abs(from), m.abs(to))
}

func (m *propModel) remoteWrite(rt *rapid.T) {
	rel := m.drawPath(rt, "path")
	if paths := m.s.paths(); len(paths) > 0 && rapid.Bool().Draw(rt, "overwrite") {
		var ps []string
		for p := range paths {
			ps = append(ps, p)
		}
		sort.Strings(ps)
		rel = rapid.SampledFrom(ps).Draw(rt, "existing")
	}
	m.s.put(rel, m.content())
}

func (m *propModel) remoteMkdir(rt *rapid.T) {
	rel := m.drawPath(rt, "dir")
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	parent := m.s.rootID
	for _, p := range strings.Split(rel, "/") {
		c := m.s.child(parent, localfs.NameKey(p), "")
		if c == nil {
			var err error
			if c, err = m.s.mkdir(parent, p); err != nil {
				return
			}
		}
		if !c.dir {
			return
		}
		parent = c.id
	}
}

func (m *propModel) remoteTrash(rt *rapid.T) {
	ids := m.s.liveIDs()
	if len(ids) == 0 {
		return
	}
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	_ = m.s.trash(rapid.SampledFrom(ids).Draw(rt, "victim"), "")
}

func (m *propModel) remoteMove(rt *rapid.T) {
	ids := m.s.liveIDs()
	if len(ids) == 0 {
		return
	}
	id := rapid.SampledFrom(ids).Draw(rt, "node")
	var dirs []string
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	dirs = append(dirs, m.s.rootID)
	for _, d := range ids {
		if n := m.s.nodes[d]; n != nil && n.dir {
			dirs = append(dirs, d)
		}
	}
	to := rapid.SampledFrom(dirs).Draw(rt, "into")
	name := rapid.SampledFrom(m.names()).Draw(rt, "name")
	_, _ = m.s.move(id, to, name, "")
}

// sync runs one engine pass, maybe with network errors and a crash in
// the middle, and checks nothing local was lost.
func (m *propModel) sync(rt *rapid.T) {
	before := localTree(rt, m.h.root)
	m.mu.Lock()
	m.pFault = rapid.SampledFrom([]float64{0, 0, 0.1, 0.3}).Draw(rt, "faults")
	m.mu.Unlock()
	crash := rapid.IntRange(0, 6).Draw(rt, "crashAt")
	m.crashK.Store(int64(crash))
	if rapid.Bool().Draw(rt, "rescan") {
		m.h.e.RescanAll()
	}
	_, err := m.h.e.Pass(context.Background())
	m.crashK.Store(0)
	m.mu.Lock()
	m.pFault = 0
	m.mu.Unlock()
	if errors.Is(err, errStop) {
		m.h.restart()
	} else if err != nil && !transient(err) {
		rt.Fatalf("pass: %v", err)
	}
	after := localTree(rt, m.h.root)
	for p, sha := range before {
		if sha == "/" || after[p] == sha {
			continue
		}
		if !m.inTree(after, sha) && !m.s.has(sha) {
			rt.Fatalf("%s (%s) disappeared from the disk and isn't on the server", p, sha[:8])
		}
	}
}

func (m *propModel) inTree(t map[string]string, sha string) bool {
	for _, v := range t {
		if v == sha {
			return true
		}
	}
	return false
}

// loseMarker: the folder is gone (a disk unplugged), an empty one in its
// place; a pass must refuse and change nothing.
func (m *propModel) loseMarker(rt *rapid.T) {
	away := m.h.root + ".away"
	if err := os.Rename(m.h.root, away); err != nil {
		rt.Fatal(err)
	}
	if err := os.Mkdir(m.h.root, 0o755); err != nil {
		rt.Fatal(err)
	}
	m.s.mu.Lock()
	seq := m.s.seq
	m.s.mu.Unlock()
	if _, err := m.h.e.Pass(context.Background()); !errors.Is(err, ErrNoMarker) {
		rt.Fatalf("pass without the marker: %v", err)
	}
	m.s.mu.Lock()
	changed := m.s.seq != seq
	m.s.mu.Unlock()
	if changed {
		rt.Fatal("the server changed during a pass without the marker")
	}
	if ents, _ := os.ReadDir(m.h.root); len(ents) != 0 {
		rt.Fatalf("wrote into the empty mount point: %v", ents)
	}
	os.Remove(m.h.root)
	if err := os.Rename(away, m.h.root); err != nil {
		rt.Fatal(err)
	}
}

// quiesce: no more faults, backoffs over; sync must converge.
func (m *propModel) quiesce(rt *rapid.T) {
	m.h.e.mu.Lock()
	m.h.e.backoff = map[string]*backoff{}
	m.h.e.mu.Unlock()
	ctx := context.Background()
	if err := m.h.db.Update(ctx, func(tx *state.Tx) error {
		ops, err := tx.Ops()
		for _, o := range ops {
			if err := tx.RetryOp(o.ID); err != nil {
				return err
			}
		}
		return err
	}); err != nil {
		rt.Fatal(err)
	}
	var res PassResult
	var err error
	for range 3 {
		res, err = m.h.e.SyncOnce(ctx)
		if err != nil {
			rt.Fatalf("final sync: %v\n%s", err, m.h.dump())
		}
		// Backoffs of the final sync's own retries (a replan after a
		// precondition) are short; clear and go again.
		m.h.e.mu.Lock()
		m.h.e.backoff = map[string]*backoff{}
		m.h.e.mu.Unlock()
	}
	// What the user wrote where they can't write stays local, a problem.
	local := localTree(rt, m.h.root)
	for _, p := range m.h.problems() {
		switch p.Kind {
		case ProblemReadOnly:
			for q := range local {
				if under(q, p.RelPath) {
					delete(local, q)
				}
			}
		case ProblemConflict, ProblemRescued, ProblemForbidden:
		default:
			rt.Fatalf("problem left: %+v", p)
		}
	}
	if d := diffTrees(local, m.s.viewAll()); d != "" {
		rt.Fatalf("not converged (last: %+v, problems %v):\n%s\n%s", res, m.h.problems(), d, m.h.dump())
	}
	if res, err := m.h.e.Pass(ctx); err != nil || res.Ops != 0 {
		rt.Fatalf("an extra pass after convergence: %+v, %v\n%s", res, err, m.h.dump())
	}
	walkParts(rt, m.h.root)
}

// walkParts: no partial downloads left behind.
func walkParts(rt *rapid.T, root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".~cloud-") {
			rt.Fatalf("left behind: %s", p)
		}
		return nil
	})
}
