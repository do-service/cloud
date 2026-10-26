package engine

import (
	"sort"
	"strings"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// The nodes base knows: each is matched with its remote state (by node
// id) and its local entry (claim), and the reconciliation table of
// docs/desktop-client.md decides. Position (folder and name) and content
// are separate questions; a node whose position changes gets only that op
// in a pass, its content follows on the next one.

// positions plans the moves and renames.
func (p *planner) positions() {
	for _, b := range p.baseSorted {
		if p.busyNode(b.NodeID) {
			p.done[b.NodeID] = true
			p.deferOne()
			continue
		}
		r, l := p.remote[b.NodeID], p.claim[b.NodeID]
		if r == nil || l == nil || !p.area.in(b.NodeID) {
			continue
		}
		bp, bok := p.baseParent(b)
		bn := nfc(nameOf(b.RelPath))
		lp, lok := p.localParent(l)
		ln := nfc(nameOf(l.RelPath))
		posR := !bok || r.ParentID != bp || r.Name != bn
		posL := !lok || lp != bp || ln != bn
		if !posR && !posL {
			continue
		}
		if lok && lp == r.ParentID && ln == r.Name && nfc(b.RelPath) == nfc(l.RelPath) {
			// Both sides agree and base has the node's path right; only
			// its folder's row says otherwise, and that one is fixed on
			// its own.
			continue
		}
		p.done[b.NodeID] = true
		if p.inMoved(b.RelPath) || p.inMoved(l.RelPath) {
			p.deferOne()
			continue
		}
		dir := b.Kind == state.KindDir
		switch {
		case posR && lok && lp == r.ParentID && ln == r.Name:
			// Both sides moved it to the same place.
			p.add(Op{Kind: OpRebase, NodeID: b.NodeID, BasePath: b.RelPath, Path: l.RelPath, Dir: dir})
			if dir {
				p.markMoved(b.RelPath, l.RelPath)
			}
		case posR && bok && r.ParentID == bp && isTempName(r.Name, p.s.Options.DeviceName):
			// This device's step aside of a cycle (breakCycle), made on
			// the server but not recorded: a crash, then a retry that
			// failed. It isn't the server's choice: base follows it, and
			// the next pass sends the local position.
			to := join(dirOf(b.RelPath), r.Name)
			p.add(Op{Kind: OpRebase, NodeID: b.NodeID, BasePath: b.RelPath, Path: to, Dir: dir})
			if dir {
				p.markMoved(b.RelPath, to)
			}
		case posR:
			// The server's position wins over a local move.
			p.moveLocal(b, r, l)
		case p.pinned(b.NodeID):
			// «Доступно мне» and the shares in it stay where the server
			// has them.
			p.moveLocal(b, r, l)
		default:
			p.moveRemote(b, r, l, lp, lok, ln)
		}
	}
}

func (p *planner) moveLocal(b *state.Base, r *state.Remote, l *state.Local) {
	td, ok := p.localPathOf(r.ParentID)
	if !ok {
		p.deferOne()
		return
	}
	target := join(td, r.Name)
	if p.inMoved(target) || below(nfc(target), nfc(l.RelPath)) {
		p.deferOne()
		return
	}
	if p.occupiedLocal(td, r.Name, l) {
		if !p.inOccupant(td, r.Name, l) {
			if cyc := p.cycle(l, p.localWait); cyc != nil {
				// What holds the name waits, through others, for this
				// one's name (the server swapped them): this one steps
				// aside on disk, and the rest follow.
				p.breakCycle(cyc)
				to := join(dirOf(l.RelPath), p.freeTemp(dirOf(l.RelPath), nameOf(l.RelPath), ""))
				p.add(Op{Kind: OpRenameLocal, NodeID: b.NodeID, From: l.RelPath, Path: to, BasePath: b.RelPath,
					Dir: b.Kind == state.KindDir, Expect: expectOf(l)})
				if b.Kind == state.KindDir {
					p.markMoved(l.RelPath, to, b.RelPath)
				}
				return
			}
			// What's there moves away or aside in this pass.
			p.deferOne()
			return
		}
		// The name is held by a folder this one is in: it can't go
		// there before leaving it. When that folder's own place is in
		// turn inside this one (a folder swapped with its parent on
		// disk, or so it looks on ext4 when a new folder gets a deleted
		// one's inode), neither could ever go back first. This one steps
		// out next to it under a temporary name; later passes put both
		// where the server has them.
		target = join(td, p.freeTemp(td, r.Name, r.ParentID))
	}
	dir := b.Kind == state.KindDir
	p.add(Op{Kind: OpRenameLocal, NodeID: b.NodeID, From: l.RelPath, Path: target, BasePath: b.RelPath, Dir: dir, Expect: expectOf(l)})
	p.take(td, nameOf(target))
	if dir {
		p.markMoved(l.RelPath, target, b.RelPath)
	}
}

// inOccupant: what takes name in local folder dir is a folder self is
// inside of.
func (p *planner) inOccupant(dir, name string, self *state.Local) bool {
	for _, o := range p.localKids[nfc(dir)][localfs.NameKey(name)] {
		if o != self && below(nfc(self.RelPath), nfc(o.RelPath)) {
			return true
		}
	}
	return false
}

func (p *planner) moveRemote(b *state.Base, r *state.Remote, l *state.Local, lp string, lok bool, ln string) {
	if !p.canSend(r, l) {
		// Without write access (a read-only share, «Доступно мне») the
		// server's position wins: the move is undone. Between trees it
		// is a copy and a delete (splitCrossRoot), here only where the
		// copy can't be made.
		p.moveLocal(b, r, l)
		return
	}
	if !lok || p.remote[lp] == nil {
		// Into a folder not on the server yet: it is created first.
		if !p.tempMove(b, r, l) {
			p.deferOne()
		}
		return
	}
	if lp == r.NodeID || p.remoteBelow(lp, r.NodeID) {
		// Moved locally into a folder the server has inside it (the
		// other side moved that one in here meanwhile): the server can't
		// make the cycle, so its arrangement wins and the folder goes
		// back to where the server has it.
		p.moveLocal(b, r, l)
		return
	}
	name, key, err := localfs.NormalizeName(ln)
	if err != nil {
		p.problem(ProblemNameInvalid, l.RelPath, b.NodeID, err.Error())
		return
	}
	if occ := p.remoteChild(lp, key); occ != nil && occ.NodeID != r.NodeID {
		if p.base[occ.NodeID] != nil && p.willVacate(occ) {
			if !p.tempMove(b, r, l) {
				p.deferOne()
			}
			return
		}
		p.renameAside(l, lp, b.NodeID)
		return
	}
	dir := b.Kind == state.KindDir
	op := Op{Kind: OpMoveRemote, NodeID: b.NodeID, ParentID: lp, Name: name, BasePath: b.RelPath, Path: l.RelPath, Dir: dir}
	if !dir {
		// "As last seen": the mirror's ETag, not base's — a content
		// change already known is the next pass's download, not a reason
		// to refuse the move. A folder's ETag changes with anything
		// inside it, and the mirror doesn't follow those (the server
		// journals the node, not its ancestors): only a file's is a
		// useful condition.
		op.IfMatch = r.ETag
	}
	p.add(op)
	if dir {
		p.markMoved(b.RelPath, l.RelPath)
	}
}

// tempMove breaks a cycle of renames the server can't make one by one:
// a and b swapped locally, or a node moved into a new folder that takes
// the name of another one moving into its own place. l's node waits,
// through the others, for its own name; it steps aside on the server, in
// its folder, under a temporary name. The others follow, and it takes its
// local name last. Base follows it there, so the temporary name isn't
// taken for a move made on the server.
func (p *planner) tempMove(b *state.Base, r *state.Remote, l *state.Local) bool {
	if !p.writable(r.ParentID) {
		return false
	}
	cyc := p.cycle(l, p.nameWait)
	if cyc == nil {
		return false
	}
	p.breakCycle(cyc)
	dir := b.Kind == state.KindDir
	parentPath := dirOf(b.RelPath)
	to := join(parentPath, p.freeTemp(parentPath, r.Name, r.ParentID))
	op := Op{Kind: OpMoveRemote, NodeID: b.NodeID, ParentID: r.ParentID, Name: nameOf(to), BasePath: b.RelPath, Path: to, Dir: dir}
	if !dir {
		op.IfMatch = r.ETag
	}
	p.add(op)
	if dir {
		p.markMoved(b.RelPath, to)
	}
	return true
}

// cycle follows wait from start: the entries of the cycle, start first,
// if it comes back to start; nil if it ends, or loops without start, or
// meets a cycle already broken in this pass.
func (p *planner) cycle(start *state.Local, wait func(*state.Local) *state.Local) []*state.Local {
	if p.broken[start] {
		return nil
	}
	path := []*state.Local{start}
	seen := map[*state.Local]bool{start: true}
	for cur := wait(start); cur != nil; cur = wait(cur) {
		if cur == start {
			return path
		}
		if seen[cur] || p.broken[cur] {
			return nil
		}
		seen[cur] = true
		path = append(path, cur)
	}
	return nil
}

func (p *planner) breakCycle(cyc []*state.Local) {
	for _, l := range cyc {
		p.broken[l] = true
	}
}

// nameWait: the local entry whose move frees the server name that l (or
// the new folder it is in, which the server doesn't have yet) needs —
// that of a node leaving it (willVacate). nil when l waits for no name.
func (p *planner) nameWait(l *state.Local) *state.Local {
	for range 4096 {
		parent, ok := p.nodeAt[nfc(dirOf(l.RelPath))]
		if !ok {
			// Its folder is new: that one's creation needs a name.
			if l = p.local[nfc(dirOf(l.RelPath))]; l == nil {
				return nil
			}
			continue
		}
		if p.remote[parent] == nil {
			return nil
		}
		_, key, err := localfs.NormalizeName(nameOf(l.RelPath))
		if err != nil {
			return nil
		}
		occ := p.remoteChild(parent, key)
		if occ == nil || p.claim[occ.NodeID] == l || p.base[occ.NodeID] == nil || !p.willVacate(occ) {
			return nil
		}
		return p.claim[occ.NodeID]
	}
	return nil
}

// localWait: the local entry holding the name on disk that l's node
// moves to locally (the server moved it, or it can't be moved there),
// if that one moves away locally too. nil otherwise.
func (p *planner) localWait(l *state.Local) *state.Local {
	id, ok := p.nodeAt[nfc(l.RelPath)]
	if !ok || p.claim[id] != l {
		return nil
	}
	r, b := p.remote[id], p.base[id]
	if r == nil || b == nil || !p.area.in(id) {
		return nil
	}
	bp, bok := p.baseParent(b)
	posR := !bok || r.ParentID != bp || r.Name != nfc(nameOf(b.RelPath))
	if !posR && !p.pinned(id) && p.canSend(r, l) {
		// Its local position goes to the server instead.
		return nil
	}
	td, ok := p.localPathOf(r.ParentID)
	for n := 0; !ok && n < 4096; n++ {
		// Into a folder new on the server: its creation here needs a
		// name.
		r = p.remote[r.ParentID]
		if r == nil || p.base[r.NodeID] != nil || !p.area.in(r.NodeID) {
			return nil
		}
		td, ok = p.localPathOf(r.ParentID)
	}
	if !ok || nfc(join(td, r.Name)) == nfc(l.RelPath) {
		return nil
	}
	occ := p.localKids[nfc(td)][keyOf(r)]
	if len(occ) != 1 || occ[0] == l {
		return nil
	}
	return occ[0]
}

// expectOf is the stamp a local rename expects at its source: its file id
// tells the entry from another one put there since.
func expectOf(l *state.Local) *localfs.Stamp {
	st := lstamp(l)
	return &st
}

// readOnlyAt: the nearest folder at or above dir is on the server and
// the user can't write to it (one gone on the server is re-created).
func (p *planner) readOnlyAt(dir string) bool {
	n := p.nearestNode(dir)
	return p.remote[n] != nil && !p.writable(n)
}

// canSend: a local move of r to where l is can be made on the server —
// from and into folders the user can write to, within one tree. The
// nearest folder above l the server has stands for one not created yet.
func (p *planner) canSend(r *state.Remote, l *state.Local) bool {
	to := p.nearestNode(dirOf(l.RelPath))
	return !p.pinned(r.NodeID) && p.writable(to) && p.writable(r.ParentID) && p.rootOf(to) == p.rootOf(r.NodeID)
}

// contents plans content and deletes for the nodes positions left.
func (p *planner) contents() {
	for _, b := range p.baseSorted {
		if p.done[b.NodeID] || p.covered[b.NodeID] {
			continue
		}
		r, l := p.remote[b.NodeID], p.claim[b.NodeID]
		if p.inMoved(b.RelPath) || l != nil && p.inMoved(l.RelPath) {
			p.done[b.NodeID] = true
			p.deferOne()
			continue
		}
		switch {
		case r == nil || !p.area.in(b.NodeID):
			p.gone(b, r, l)
		case l == nil:
			p.localDeleted(b, r)
		case b.Kind == state.KindFile:
			p.file(b, r, l)
		}
	}
}

// file: both sides have it where base has it.
func (p *planner) file(b *state.Base, r *state.Remote, l *state.Local) {
	lc, lt, ok := p.localChange(b, l)
	if !ok {
		p.deferOne()
		return
	}
	rc := remoteChanged(r, b)
	switch {
	case lc && rc:
		if l.SHA256Cached == r.SHA256 {
			p.setBase(b, r, l)
		} else {
			p.conflict(b, r, l)
		}
	case lc && !p.writable(r.NodeID):
		// Read-only: the edit is kept aside, the server's version comes
		// back under the name.
		p.conflict(b, r, l)
	case lc:
		st := lstamp(l)
		p.add(Op{Kind: OpUpload, NodeID: b.NodeID, Path: l.RelPath, IfMatch: b.ETag,
			SHA256: l.SHA256Cached, Size: l.Size, MtimeNS: l.MtimeNS, Expect: &st})
	case rc:
		st := lstamp(l)
		p.add(Op{Kind: OpDownload, NodeID: b.NodeID, Path: l.RelPath, ETag: r.ETag, SHA256: r.SHA256,
			Size: r.Size, MtimeNS: r.MtimeNS, Expect: &st})
	case lt || r.ETag != b.ETag:
		p.setBase(b, r, l)
	}
}

// setBase records that both sides have the same content: the local
// stamp and the server's ETag.
func (p *planner) setBase(b *state.Base, r *state.Remote, l *state.Local) {
	nb := *b
	nb.RelPath, nb.FileID, nb.Size, nb.MtimeNS = l.RelPath, l.FileID, l.Size, l.MtimeNS
	nb.ETag = r.ETag
	if r.SHA256 != "" {
		nb.SHA256 = r.SHA256
	}
	p.add(Op{Kind: OpSetBase, NodeID: b.NodeID, Path: l.RelPath, Base: &nb})
}

// conflict moves the local edit aside; the next pass downloads the
// server's version to the name and uploads the copy as a new file.
func (p *planner) conflict(b *state.Base, r *state.Remote, l *state.Local) {
	dir := dirOf(l.RelPath)
	to := join(dir, p.freeName(dir, nameOf(l.RelPath), r.ParentID))
	p.add(Op{Kind: OpConflict, NodeID: b.NodeID, From: l.RelPath, Path: to})
}

// localDeleted: gone from disk, still on the server in the selected area.
func (p *planner) localDeleted(b *state.Base, r *state.Remote) {
	if p.keepShare(r) {
		p.restore(b, r)
		return
	}
	if b.Kind == state.KindFile {
		if remoteChanged(r, b) || !p.writable(r.ParentID) {
			// An edit on the server beats the local delete; so does a
			// folder the user can't delete from.
			p.restore(b, r)
			return
		}
		p.add(Op{Kind: OpTrashRemote, NodeID: b.NodeID, Path: b.RelPath, IfMatch: b.ETag, Deletes: 1})
		return
	}
	if p.isDeletable(r.NodeID) && p.writable(r.ParentID) {
		n := p.cover(r.NodeID)
		st := bstamp(b)
		p.add(Op{Kind: OpTrashRemote, NodeID: b.NodeID, Path: b.RelPath, Dir: true, Deletes: n, Expect: &st})
		return
	}
	if p.pendingTrash(r.NodeID) {
		// Its known files go to the trash one by one first.
		p.deferOne()
		return
	}
	// Something in it stays (new or edited on the server): the folder
	// comes back.
	p.restore(b, r)
}

// restore downloads a file, or re-creates a folder, the user deleted
// locally, at the server's position.
func (p *planner) restore(b *state.Base, r *state.Remote) {
	td, ok := p.localPathOf(r.ParentID)
	if !ok {
		p.deferOne()
		return
	}
	target := join(td, r.Name)
	if p.inMoved(target) || p.occupiedLocal(td, r.Name, nil) {
		p.stepAside(td, r)
		p.deferOne()
		return
	}
	p.take(td, r.Name)
	if b.Kind == state.KindDir {
		p.add(Op{Kind: OpMkdirLocal, NodeID: b.NodeID, Path: target, Dir: true})
		p.plannedLocal[b.NodeID] = target
		return
	}
	p.add(Op{Kind: OpDownload, NodeID: b.NodeID, Path: target, ETag: r.ETag, SHA256: r.SHA256, Size: r.Size, MtimeNS: r.MtimeNS})
}

// isDeletable: a folder deleted locally whose server subtree holds only
// what the client synced, unchanged since, and deleted locally too. Then
// the folder goes to the trash as a whole; otherwise its files go one by
// one and what's left comes back.
func (p *planner) isDeletable(id string) bool {
	if v, ok := p.deletable[id]; ok {
		return v == 1
	}
	p.deletable[id] = 0 // a cycle in a corrupt mirror isn't deletable
	ok := true
	for _, c := range p.kids[id] {
		cb := p.base[c.NodeID]
		if cb == nil || p.claim[c.NodeID] != nil || !p.area.in(c.NodeID) || p.busyNode(c.NodeID) ||
			c.Kind == state.KindFile && remoteChanged(c, cb) ||
			c.Kind == state.KindDir && !p.isDeletable(c.NodeID) {
			ok = false
			break
		}
	}
	if ok {
		p.deletable[id] = 1
	}
	return ok
}

// pendingTrash: a child of id is trashed (or trashable) on its own, after
// which id is looked at again.
func (p *planner) pendingTrash(id string) bool {
	for _, c := range p.kids[id] {
		cb := p.base[c.NodeID]
		if cb == nil || p.claim[c.NodeID] != nil || !p.area.in(c.NodeID) {
			continue
		}
		if c.Kind == state.KindFile && !remoteChanged(c, cb) && p.writable(id) {
			return true
		}
		// As localDeleted trashes them: a folder only from one the user
		// can write to.
		if c.Kind == state.KindDir && (p.isDeletable(c.NodeID) && p.writable(id) || p.pendingTrash(c.NodeID)) {
			return true
		}
	}
	return false
}

// cover marks id's known subtree as handled by an op on id and counts
// its files.
func (p *planner) cover(id string) int {
	n := 0
	for _, c := range p.kids[id] {
		if p.base[c.NodeID] == nil {
			continue
		}
		p.covered[c.NodeID] = true
		if c.Kind == state.KindFile {
			n++
		} else {
			n += p.cover(c.NodeID)
		}
	}
	return n
}

// willVacate: a node base knows, at a name on the server, leaves that
// name in this pass or the next — moved away locally, or deleted locally
// and about to be trashed.
func (p *planner) willVacate(occ *state.Remote) bool {
	ob := p.base[occ.NodeID]
	if ob == nil || !p.area.in(occ.NodeID) || p.pinned(occ.NodeID) {
		// «Доступно мне» and the shares keep their names: moved away
		// locally, they come back.
		return false
	}
	if p.busyNode(occ.NodeID) {
		return true
	}
	bp, bok := p.baseParent(ob)
	if !bok || occ.ParentID != bp || occ.Name != nfc(nameOf(ob.RelPath)) {
		// The server put it there: it stays.
		return false
	}
	cl := p.claim[occ.NodeID]
	if cl == nil {
		if p.keepShare(occ) {
			return false
		}
		if ob.Kind == state.KindFile {
			return !remoteChanged(occ, ob) && p.writable(occ.ParentID)
		}
		return p.isDeletable(occ.NodeID) && p.writable(occ.ParentID)
	}
	lp, lok := p.localParent(cl)
	if lok && (lp == occ.NodeID || p.remoteBelow(lp, occ.NodeID)) || !p.canSend(occ, cl) {
		// Its local move would be a cycle on the server, or can't be
		// made there: it comes back.
		return false
	}
	return !lok || lp != bp || localfs.NameKey(nameOf(cl.RelPath)) != localfs.NameKey(nameOf(ob.RelPath))
}

// gone: no longer on the server in the selected area — deleted there,
// moved out, or unchecked.
func (p *planner) gone(b *state.Base, r *state.Remote, l *state.Local) {
	if l == nil {
		p.add(Op{Kind: OpForget, NodeID: b.NodeID, Path: b.RelPath})
		return
	}
	deleted := r == nil
	if b.Kind == state.KindFile {
		lc, _, ok := p.localChange(b, l)
		if !ok {
			p.deferOne()
			return
		}
		bp, _ := p.baseParent(b)
		lp, lok := p.localParent(l)
		moved := !lok || lp != bp || nfc(nameOf(l.RelPath)) != nfc(nameOf(b.RelPath))
		switch {
		case deleted && (lc || moved) && (p.inDead(l.RelPath) || p.readOnlyAt(dirOf(l.RelPath))):
			// It can't go up where it is (a share gone, or read-only, the
			// file moved in there): it goes to the user's own tree.
			p.rescue(l, b.NodeID)
		case deleted && (lc || moved):
			// An edit (or a move) beats the server's delete.
			p.uploadNew(l, b)
		case deleted:
			p.add(Op{Kind: OpRemoveLocal, NodeID: b.NodeID, Path: l.RelPath,
				Items: []RemoveItem{{Path: l.RelPath, NodeID: b.NodeID, Expect: lstamp(l)}}, Deletes: 1})
		case lc && !p.writable(r.NodeID):
			p.problem(ProblemUnselectKept, l.RelPath, b.NodeID, "edited, but the file is read-only on the server")
		case lc && remoteChanged(r, b) && l.SHA256Cached == r.SHA256:
			p.setBase(b, r, l)
		case lc && remoteChanged(r, b):
			p.conflict(b, r, l)
		case lc:
			// Unchecked with an edit not uploaded yet: upload it first.
			st := lstamp(l)
			p.add(Op{Kind: OpUpload, NodeID: b.NodeID, Path: l.RelPath, IfMatch: b.ETag,
				SHA256: l.SHA256Cached, Size: l.Size, MtimeNS: l.MtimeNS, Expect: &st})
		default:
			p.add(Op{Kind: OpRemoveLocal, NodeID: b.NodeID, Path: l.RelPath,
				Items: []RemoveItem{{Path: l.RelPath, NodeID: b.NodeID, Expect: lstamp(l)}}})
		}
		return
	}
	if items, files, ok := p.removableTree(l.RelPath); ok {
		if !deleted {
			files = 0
		}
		p.add(Op{Kind: OpRemoveLocal, NodeID: b.NodeID, Path: l.RelPath, Items: items, Dir: true, Deletes: files})
		return
	}
	if deleted && p.hasKeepers(l.RelPath) && !p.inDead(l.RelPath) {
		p.recreateRemote(b, l)
		return
	}
	// Its contents are being uploaded or removed first.
	p.deferOne()
}

// uploadNew uploads a known file as a new one: its node is gone on the
// server but the local copy has something to keep.
func (p *planner) uploadNew(l *state.Local, b *state.Base) {
	sha := l.SHA256Cached
	if sha == "" && lstamp(l).Same(bstamp(b)) {
		sha = b.SHA256
	}
	if sha == "" {
		p.deferOne()
		return
	}
	p.createRemote(l, sha, b.NodeID)
}

// recreateRemote: a folder deleted on the server with local changes in
// it comes back there, holding them.
func (p *planner) recreateRemote(b *state.Base, l *state.Local) {
	parent, ok := p.localParent(l)
	if !ok || p.remote[parent] == nil {
		p.deferOne()
		return
	}
	_, key, err := localfs.NormalizeName(nameOf(l.RelPath))
	if err == nil && p.remoteChild(parent, key) != nil {
		// The name is taken on the server again: forget the old node, and
		// the folder pairs up with the new one as new.
		p.add(Op{Kind: OpForget, NodeID: b.NodeID, Path: b.RelPath})
		return
	}
	p.createRemote(l, "", b.NodeID)
}

// removableTree: everything on disk at and below rel is a known node
// gone from the server's selected area, unchanged since the last sync.
// The items are deepest first.
func (p *planner) removableTree(rel string) ([]RemoveItem, int, bool) {
	var items []RemoveItem
	files := 0
	for _, l := range p.subtree(rel, true) {
		id, ok := p.nodeAt[nfc(l.RelPath)]
		if !ok || p.busyNode(id) {
			return nil, 0, false
		}
		if r := p.remote[id]; r != nil && p.area.in(id) {
			return nil, 0, false
		}
		b := p.base[id]
		if l.Kind == state.KindFile {
			if lc, _, ok := p.localChange(b, l); !ok || lc {
				return nil, 0, false
			}
			files++
		}
		items = append(items, RemoveItem{Path: l.RelPath, NodeID: id, Expect: lstamp(l)})
	}
	for _, it := range items {
		p.covered[it.NodeID] = true
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path > items[j].Path })
	return items, files, true
}

// subtree lists the local entries below rel (and rel itself, with self).
// Below "x" means a "x/" prefix: in byte order "x (2)" sorts between "x"
// and "x/…", so the range starts at "x/".
func (p *planner) subtree(rel string, self bool) []*state.Local {
	rel = nfc(rel)
	var out []*state.Local
	if l := p.local[rel]; self && l != nil {
		out = append(out, l)
	}
	prefix := rel + "/"
	i := sort.Search(len(p.localSorted), func(i int) bool { return nfc(p.localSorted[i].RelPath) >= prefix })
	for ; i < len(p.localSorted) && strings.HasPrefix(nfc(p.localSorted[i].RelPath), prefix); i++ {
		out = append(out, p.localSorted[i])
	}
	return out
}

// hasKeepers: below rel is something new or edited locally, or a node
// still on the server that the user moved in here.
func (p *planner) hasKeepers(rel string) bool {
	for _, l := range p.subtree(rel, false) {
		id, ok := p.nodeAt[nfc(l.RelPath)]
		if !ok || p.remote[id] != nil && p.area.in(id) {
			return true
		}
		if b := p.base[id]; l.Kind == state.KindFile && b != nil {
			if lc, _, ok := p.localChange(b, l); ok && lc {
				return true
			}
		}
	}
	return false
}
