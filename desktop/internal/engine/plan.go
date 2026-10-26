package engine

import (
	"sort"
	"strings"
	"time"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// Snapshot is everything a plan is made from: the three states of the
// three-way reconciliation (docs/desktop-client.md, «Движок») and the
// selective-sync rules. Plan is a pure function of it.
type Snapshot struct {
	// RootID is the own root's node id, the sync folder itself.
	RootID string
	// Remote is the mirror of the server's tree.
	Remote []state.Remote
	// Base is what both sides last agreed on.
	Base []state.Base
	// Local is the last scan, with hashes where the engine computed them
	// (SHA256Cached, valid for the entry's stamp).
	Local []state.Local
	Rules map[string]state.RuleMode
	// BusyNodes and BusyPaths have an op in the journal or backing off:
	// the plan leaves them alone.
	BusyNodes map[string]bool
	BusyPaths map[string]bool
	// Adopted: the top-level names the sync folder held when it was
	// connected (state.Tx.Adopted), in NFC; see planner.leftAlone.
	Adopted map[string]bool
	// Now dates conflict copies and tells files still being written.
	Now     time.Time
	Options Options
}

type Options struct {
	// DeviceName goes into conflict copies' names.
	DeviceName string
	// WindowsNames: don't download names Windows can't store (on Windows).
	WindowsNames bool
	// StableAfter: a file modified more recently than this is still being
	// written; it isn't uploaded, and nothing overwrites it.
	StableAfter time.Duration
}

// Plan is one pass's worth of ops. The executor runs them by phase; what
// depends on their results (a file in a folder created in this pass) is
// planned on the next pass, so a pass is never more than a step, and the
// engine repeats passes until one plans nothing.
type Plan struct {
	Ops      []Op
	Problems []state.Problem
	// Deferred counts what waits for another op of this pass, a file
	// still being written, or a busy node.
	Deferred int
	// Unstable counts the files deferred for being written right now.
	Unstable int
}

// Deletes is how many files the plan's ops make disappear on the other
// side (Op.Deletes).
func (p Plan) Deletes() int {
	n := 0
	for _, o := range p.Ops {
		n += o.Deletes
	}
	return n
}

// MakePlan reconciles a snapshot.
func MakePlan(s *Snapshot) Plan {
	p := newPlanner(s)
	p.match()
	p.findDead()
	p.positions()
	p.contents()
	p.newLocal()
	p.newRemote()
	p.sortOps()
	return p.plan
}

type planner struct {
	s      *Snapshot
	remote map[string]*state.Remote
	// kids: the mirror's children by parent and by name key.
	kids  map[string][]*state.Remote
	kidAt map[string]map[string]*state.Remote
	area  *area

	base       map[string]*state.Base
	baseAt     map[string]*state.Base
	baseSorted []*state.Base

	local       map[string]*state.Local
	localSorted []*state.Local
	// localKids: local entries by folder and name key, to find what takes
	// a name on a case-insensitive disk.
	localKids map[string]map[string][]*state.Local

	// claim: node → its local entry now; nodeAt the reverse, by path.
	claim     map[string]*state.Local
	nodeAt    map[string]string
	claimed   map[*state.Local]bool
	unclaimed []*state.Local

	done    map[string]bool
	covered map[string]bool
	// moved: local paths of folders a move of this pass changes; what's
	// below them waits for the next pass.
	moved []string
	// plannedLocal: folders created locally in this pass, so their
	// contents can be planned at once.
	plannedLocal map[string]string
	// taken: names (by key) in local folders that ops of this pass claim.
	taken     map[string]map[string]bool
	deletable map[string]int8
	// renamed: nodes stepped aside in this pass.
	renamed map[string]bool
	// broken: the local entries of a cycle of renames one of which steps
	// to a temporary name in this pass (plan_known.go, breakCycle).
	broken map[*state.Local]bool
	// dead: local paths of shares gone from the server (shared.go).
	dead []string

	plan Plan
}

func newPlanner(s *Snapshot) *planner {
	p := &planner{
		s: s, remote: map[string]*state.Remote{}, kids: map[string][]*state.Remote{}, kidAt: map[string]map[string]*state.Remote{},
		base: map[string]*state.Base{}, baseAt: map[string]*state.Base{},
		local: map[string]*state.Local{}, localKids: map[string]map[string][]*state.Local{},
		claim: map[string]*state.Local{}, nodeAt: map[string]string{"": s.RootID}, claimed: map[*state.Local]bool{},
		done: map[string]bool{}, covered: map[string]bool{}, plannedLocal: map[string]string{},
		taken: map[string]map[string]bool{}, deletable: map[string]int8{}, renamed: map[string]bool{},
		broken: map[*state.Local]bool{},
	}
	for i := range s.Remote {
		r := &s.Remote[i]
		p.remote[r.NodeID] = r
		if r.NodeID == s.RootID {
			continue
		}
		p.kids[r.ParentID] = append(p.kids[r.ParentID], r)
		m := p.kidAt[r.ParentID]
		if m == nil {
			m = map[string]*state.Remote{}
			p.kidAt[r.ParentID] = m
		}
		m[keyOf(r)] = r
	}
	for _, ks := range p.kids {
		sort.Slice(ks, func(i, j int) bool { return keyOf(ks[i]) < keyOf(ks[j]) })
	}
	p.area = newArea(s.RootID, p.remote, s.Rules)
	for i := range s.Base {
		b := &s.Base[i]
		p.base[b.NodeID] = b
		p.baseAt[nfc(b.RelPath)] = b
		p.baseSorted = append(p.baseSorted, b)
	}
	sort.Slice(p.baseSorted, func(i, j int) bool { return p.baseSorted[i].RelPath < p.baseSorted[j].RelPath })
	for i := range s.Local {
		l := &s.Local[i]
		p.local[nfc(l.RelPath)] = l
		p.localSorted = append(p.localSorted, l)
		d := nfc(dirOf(l.RelPath))
		m := p.localKids[d]
		if m == nil {
			m = map[string][]*state.Local{}
			p.localKids[d] = m
		}
		k := localfs.NameKey(nameOf(l.RelPath))
		m[k] = append(m[k], l)
	}
	sort.Slice(p.localSorted, func(i, j int) bool { return nfc(p.localSorted[i].RelPath) < nfc(p.localSorted[j].RelPath) })
	return p
}

func keyOf(r *state.Remote) string {
	if r.NameKey != "" {
		return r.NameKey
	}
	return localfs.NameKey(r.Name)
}

// match finds each base node's local entry: at its path with its file id;
// else by file id elsewhere (moved, renamed); else at its path with
// another id (replaced by an editor's save-and-rename). What no node
// claims is new.
func (p *planner) match() {
	for _, b := range p.baseSorted {
		if l := p.local[nfc(b.RelPath)]; l != nil && l.Kind == b.Kind && (b.FileID == "" || l.FileID == b.FileID) {
			p.claimFor(b, l)
		}
	}
	byID := map[string][]*state.Local{}
	for _, l := range p.localSorted {
		if !p.claimed[l] && l.FileID != "" {
			byID[l.FileID] = append(byID[l.FileID], l)
		}
	}
	for _, b := range p.baseSorted {
		if p.claim[b.NodeID] != nil || b.FileID == "" {
			continue
		}
		// Two entries with one id are hard links: no telling which is the
		// moved one, so neither is.
		if c := byID[b.FileID]; len(c) == 1 && !p.claimed[c[0]] && c[0].Kind == b.Kind && !p.reused(b, c[0]) {
			p.claimFor(b, c[0])
		}
	}
	for _, b := range p.baseSorted {
		if p.claim[b.NodeID] != nil {
			continue
		}
		if l := p.local[nfc(b.RelPath)]; l != nil && !p.claimed[l] && l.Kind == b.Kind {
			p.claimFor(b, l)
		}
	}
	p.splitCrossRoot()
	for _, l := range p.localSorted {
		if !p.claimed[l] {
			p.unclaimed = append(p.unclaimed, l)
		}
	}
}

// reused: l, which has b's file id, more likely got it from the disk
// reusing a freed inode (ext4 does, at once) than by being b moved: b
// is gone from the server (a node the client still tracks, like a share
// no longer shared), and l sits exactly at the path of another node of
// its kind, live on the server, which keeps it by path (replaced, as an
// editor's save does). A node still on the server always follows its
// file id, so a rename is told as before.
func (p *planner) reused(b *state.Base, l *state.Local) bool {
	if p.remote[b.NodeID] != nil {
		return false
	}
	o := p.baseAt[nfc(l.RelPath)]
	return o != nil && o.NodeID != b.NodeID && o.Kind == l.Kind && p.remote[o.NodeID] != nil
}

// splitCrossRoot unpairs what was moved locally from one tree to another
// (the own one, a share, another share) into a folder the user can write
// to: the server moves nothing between owners (409 cross_owner_move), so
// the entry goes up as new where it is now, and its node is deleted from
// where it was — or comes back there, if that is read-only. Into a
// read-only place the move is undone instead (positions).
func (p *planner) splitCrossRoot() {
	for _, l := range p.localSorted {
		if !p.claimed[l] {
			continue
		}
		id := p.nodeAt[nfc(l.RelPath)]
		if p.remote[id] == nil || p.pinned(id) || p.busyNode(id) {
			continue
		}
		to := p.nearestNode(dirOf(l.RelPath))
		if p.rootOf(to) == p.rootOf(id) || !p.writable(to) {
			continue
		}
		// Parents come first, so what's below an unpaired folder finds
		// its nearest known folder above it.
		delete(p.claim, id)
		delete(p.claimed, l)
		delete(p.nodeAt, nfc(l.RelPath))
	}
}

// pinned: «Доступно мне» and the shares in it, which the user can't move
// or delete on the server.
func (p *planner) pinned(id string) bool {
	if id == SharedID {
		return true
	}
	r := p.remote[id]
	return r != nil && r.ParentID == SharedID
}

// rootOf is the top of the tree a node is in: RootID for the own one, a
// share's node, SharedID for «Доступно мне» itself, "" for a node whose
// chain the mirror doesn't have.
func (p *planner) rootOf(id string) string {
	for cur, n := id, 0; n < 4096; n++ {
		if cur == p.s.RootID || cur == SharedID {
			return cur
		}
		r := p.remote[cur]
		if r == nil {
			return ""
		}
		if r.ParentID == SharedID {
			return cur
		}
		cur = r.ParentID
	}
	return ""
}

// nearestNode is the node of the nearest local folder at or above dir
// that is paired with one.
func (p *planner) nearestNode(dir string) string {
	for d := nfc(dir); ; d = dirOf(d) {
		if id, ok := p.nodeAt[d]; ok {
			return id
		}
		if d == "" {
			return p.s.RootID
		}
	}
}

// keepShare: the node can't be deleted from the server by deleting it
// locally — «Доступно мне», a share, or what's in one the user deleted
// as a whole. Unchecking is how a share leaves the disk; deleting its
// folder doesn't empty it for everyone, it comes back.
func (p *planner) keepShare(r *state.Remote) bool {
	for cur, n := r, 0; cur != nil && n < 4096; cur, n = p.remote[cur.ParentID], n+1 {
		if cur.NodeID == p.s.RootID {
			return false
		}
		if p.pinned(cur.NodeID) && (cur.NodeID == r.NodeID || p.base[cur.NodeID] != nil && p.claim[cur.NodeID] == nil) {
			return true
		}
	}
	return false
}

// findDead finds the folders of shares the server no longer has for the
// user: base has them in «Доступно мне», the mirror doesn't. What was
// synced there goes from the disk; what the user wrote there and the
// server doesn't have is moved to their own tree (rescue).
func (p *planner) findDead() {
	vb := p.base[SharedID]
	if vb == nil {
		return
	}
	dir := nfc(vb.RelPath)
	for _, b := range p.baseSorted {
		if p.remote[b.NodeID] != nil || nfc(dirOf(b.RelPath)) != dir {
			continue
		}
		p.dead = append(p.dead, nfc(b.RelPath))
		if l := p.claim[b.NodeID]; l != nil && nfc(l.RelPath) != nfc(b.RelPath) {
			p.dead = append(p.dead, nfc(l.RelPath))
		}
	}
}

func (p *planner) inDead(rel string) bool {
	rel = nfc(rel)
	for _, d := range p.dead {
		if under(rel, d) {
			return true
		}
	}
	return false
}

// rescue moves a local entry the server can't take where it is (in a
// dead share, or one gone on the server and moved into a read-only
// share) into the sync folder's root under a conflict name; nodeID is
// its base node, gone, if any. Only the
// topmost entry to keep moves: what's below it goes along.
func (p *planner) rescue(l *state.Local, nodeID string) {
	if _, ok := p.nodeAt[nfc(dirOf(l.RelPath))]; !ok {
		p.deferOne()
		return
	}
	if !p.stable(l) {
		p.plan.Unstable++
		p.deferOne()
		return
	}
	to := p.freeName("", nameOf(l.RelPath), p.s.RootID)
	p.add(Op{Kind: OpRescue, NodeID: nodeID, From: l.RelPath, Path: to, Dir: l.Kind == state.KindDir})
	if l.Kind == state.KindDir {
		p.markMoved(l.RelPath, to)
	}
}

func (p *planner) claimFor(b *state.Base, l *state.Local) {
	p.claim[b.NodeID] = l
	p.claimed[l] = true
	p.nodeAt[nfc(l.RelPath)] = b.NodeID
}

func (p *planner) add(o Op) {
	if o.NodeID != "" {
		p.done[o.NodeID] = true
	}
	p.plan.Ops = append(p.plan.Ops, o)
}

func (p *planner) deferOne() { p.plan.Deferred++ }

func (p *planner) problem(kind, relPath, nodeID, detail string) {
	p.plan.Problems = append(p.plan.Problems, state.Problem{Kind: kind, RelPath: relPath, NodeID: nodeID, Detail: detail})
}

func (p *planner) busyNode(id string) bool { return p.s.BusyNodes[id] }
func (p *planner) busyPath(rel string) bool {
	return p.s.BusyPaths[rel]
}

// inMoved: below a folder a move of this pass changes.
func (p *planner) inMoved(rel string) bool {
	rel = nfc(rel)
	for _, m := range p.moved {
		if below(rel, m) {
			return true
		}
	}
	return false
}

func (p *planner) markMoved(paths ...string) {
	for _, m := range paths {
		p.moved = append(p.moved, nfc(m))
	}
}

func (p *planner) take(dir, name string) {
	dir = nfc(dir)
	m := p.taken[dir]
	if m == nil {
		m = map[string]bool{}
		p.taken[dir] = m
	}
	m[localfs.NameKey(name)] = true
}

func (p *planner) isTaken(dir, name string) bool { return p.taken[nfc(dir)][localfs.NameKey(name)] }

// occupiedLocal: something on disk (other than self) or an op of this
// pass has name in dir.
func (p *planner) occupiedLocal(dir, name string, self *state.Local) bool {
	for _, l := range p.localKids[nfc(dir)][localfs.NameKey(name)] {
		if l != self {
			return true
		}
	}
	return p.isTaken(dir, name)
}

func (p *planner) remoteChild(parentID, key string) *state.Remote { return p.kidAt[parentID][key] }

// localPathOf is where a node is on disk now, or is put by this pass.
func (p *planner) localPathOf(id string) (string, bool) {
	if id == p.s.RootID {
		return "", true
	}
	if l := p.claim[id]; l != nil {
		return l.RelPath, true
	}
	rel, ok := p.plannedLocal[id]
	return rel, ok
}

// baseParent is the node of the folder a base row is in.
func (p *planner) baseParent(b *state.Base) (string, bool) {
	d := dirOf(b.RelPath)
	if d == "" {
		return p.s.RootID, true
	}
	if pb := p.baseAt[nfc(d)]; pb != nil {
		return pb.NodeID, true
	}
	return "", false
}

// localParent is the node of the folder a local entry is in; false when
// that folder is new (not on the server yet).
func (p *planner) localParent(l *state.Local) (string, bool) {
	id, ok := p.nodeAt[nfc(dirOf(l.RelPath))]
	return id, ok
}

func (p *planner) writable(id string) bool {
	r := p.remote[id]
	return r != nil && r.Perms.Has(state.PermWrite)
}

// stable: not modified within StableAfter, so not being written now.
func (p *planner) stable(l *state.Local) bool {
	return l.Kind != state.KindFile || p.s.Now.Sub(time.Unix(0, l.MtimeNS)) >= p.s.Options.StableAfter
}

// includeFor says whether something created locally in folder parentID
// needs an include rule to be synced: in a container or the root, yes;
// in an included folder it is already; in one being unchecked it isn't
// meant to stay.
func (p *planner) includeFor(parentID string) bool {
	st := p.area.status(parentID)
	return st != included && (parentID == p.s.RootID || st == container)
}

func lstamp(l *state.Local) localfs.Stamp {
	id, _ := localfs.ParseFileID(l.FileID)
	return localfs.Stamp{Kind: localfs.Kind(l.Kind), Size: l.Size, MtimeNS: l.MtimeNS, FileID: id}
}

func bstamp(b *state.Base) localfs.Stamp {
	id, _ := localfs.ParseFileID(b.FileID)
	return localfs.Stamp{Kind: localfs.Kind(b.Kind), Size: b.Size, MtimeNS: b.MtimeNS, FileID: id}
}

// remoteChanged: the file's content on the server isn't base's. A new
// ETag with the same SHA-256 (a resync, a re-upload of the same bytes) is
// no change.
func remoteChanged(r *state.Remote, b *state.Base) bool {
	return r.ETag != b.ETag && (r.SHA256 == "" || b.SHA256 == "" || r.SHA256 != b.SHA256)
}

// localChange compares a claimed file with base: changed (other
// content), touched (new stamp, same content); ok false when that can't
// be told yet (being written, or not hashed).
func (p *planner) localChange(b *state.Base, l *state.Local) (changed, touched, ok bool) {
	if lstamp(l).Same(bstamp(b)) {
		return false, false, true
	}
	if !p.stable(l) || l.SHA256Cached == "" {
		if !p.stable(l) {
			p.plan.Unstable++
		}
		return false, false, false
	}
	if l.SHA256Cached == b.SHA256 {
		return false, true, true
	}
	return true, false, true
}

// freeName is a conflict copy's name for name in local folder dir
// (remote folder parentID, "" if none), free on both sides.
func (p *planner) freeName(dir, name, parentID string) string {
	for n := 1; ; n++ {
		c := conflictName(name, p.s.Options.DeviceName, p.s.Now, n)
		k := localfs.NameKey(c)
		if len(p.localKids[nfc(dir)][k]) == 0 && !p.isTaken(dir, c) && (parentID == "" || p.remoteChild(parentID, k) == nil) {
			p.take(dir, c)
			return c
		}
	}
}

// freeTemp is a tempName for name in local folder dir (remote folder
// parentID), free on both sides and in base.
func (p *planner) freeTemp(dir, name, parentID string) string {
	for n := 1; ; n++ {
		c := tempName(name, p.s.Options.DeviceName, p.s.Now, n)
		k := localfs.NameKey(c)
		if len(p.localKids[nfc(dir)][k]) == 0 && !p.isTaken(dir, c) && p.remoteChild(parentID, k) == nil &&
			p.baseAt[nfc(join(dir, c))] == nil {
			p.take(dir, c)
			return c
		}
	}
}

// renameAside moves a local entry out of the way of a server name under a
// conflict name; base doesn't follow (KeepBase), so the next pass carries
// the new name to the server.
func (p *planner) renameAside(l *state.Local, parentID, nodeID string) {
	dir := dirOf(l.RelPath)
	to := join(dir, p.freeName(dir, nameOf(l.RelPath), parentID))
	p.add(Op{Kind: OpRenameLocal, NodeID: nodeID, From: l.RelPath, Path: to, KeepBase: true, Dir: l.Kind == state.KindDir})
	if l.Kind == state.KindDir {
		p.markMoved(l.RelPath, to)
	}
}

func (p *planner) sortOps() {
	depth := func(s string) int { return strings.Count(s, "/") }
	sort.SliceStable(p.plan.Ops, func(i, j int) bool {
		a, b := p.plan.Ops[i], p.plan.Ops[j]
		if a.Kind.phase() != b.Kind.phase() {
			return a.Kind.phase() < b.Kind.phase()
		}
		da, db := depth(a.Path), depth(b.Path)
		if da != db {
			// Folders before their contents, but deletes from the bottom.
			if a.Kind.phase() == 2 {
				return da > db
			}
			return da < db
		}
		return a.Path < b.Path
	})
}
