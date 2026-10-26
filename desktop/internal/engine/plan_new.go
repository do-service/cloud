package engine

import (
	"runtime"
	"strings"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// What only one side has: local entries no node claims, and remote nodes
// of the selected area base doesn't know.

// newLocal plans uploads and folder creations, parents first: a folder
// created on the server in this pass gets its contents on the next.
func (p *planner) newLocal() {
	for _, l := range p.unclaimed {
		if p.busyPath(l.RelPath) || p.inMoved(l.RelPath) {
			p.deferOne()
			continue
		}
		if p.inDead(l.RelPath) {
			// Written in a share that is gone: kept in the own tree.
			p.rescue(l, "")
			continue
		}
		if p.leftAlone(l) {
			continue
		}
		parent, ok := p.nodeAt[nfc(dirOf(l.RelPath))]
		if !ok || p.remote[parent] == nil {
			// In a folder that is new itself, or being re-created.
			p.deferOne()
			continue
		}
		name, key, err := localfs.NormalizeName(nameOf(l.RelPath))
		if err != nil {
			p.problem(ProblemNameInvalid, l.RelPath, "", err.Error())
			continue
		}
		if p.caseLoser(l, key) {
			p.problem(ProblemCaseCollision, l.RelPath, "", "another file in this folder differs only in case or Unicode form")
			continue
		}
		if occ := p.remoteChild(parent, key); occ != nil {
			switch {
			case p.base[occ.NodeID] == nil:
				p.pair(l, occ, parent)
			case p.willVacate(occ) && !p.movesInto(occ, l):
				p.deferOne()
			default:
				p.renameAside(l, parent, "")
			}
			continue
		}
		if p.isTaken(dirOf(l.RelPath), name) {
			p.deferOne()
			continue
		}
		if !p.writable(parent) {
			p.problem(ProblemReadOnly, l.RelPath, "", "a new file in a folder without write access is not uploaded")
			continue
		}
		if l.Kind == state.KindFile && (!p.stable(l) || l.SHA256Cached == "") {
			if !p.stable(l) {
				p.plan.Unstable++
			}
			p.deferOne()
			continue
		}
		p.createRemote(l, l.SHA256Cached, "")
	}
}

// leftAlone: an entry that was in the sync folder when it was connected
// (Snapshot.Adopted: a reinstall, a folder chosen with files in it),
// where something new would get an include rule (the root, a container),
// and whose cloud counterpart isn't checked or which has none. It stays
// on disk only: connecting a folder neither uploads what it held nor
// checks the cloud's items of the same names. Once the user checks one,
// it is paired (pair): the same content becomes base, other content a
// conflict copy; inside a checked folder, the rest is new as usual.
func (p *planner) leftAlone(l *state.Local) bool {
	top, _, _ := strings.Cut(l.RelPath, "/")
	if !p.s.Adopted[nfc(top)] {
		return false
	}
	dir := dirOf(l.RelPath)
	parent, ok := p.nodeAt[nfc(dir)]
	if !ok {
		// In a folder the cloud doesn't have (yet): as that folder is.
		d := p.local[nfc(dir)]
		return d != nil && p.leftAlone(d)
	}
	if !p.includeFor(parent) {
		return false
	}
	_, key, err := localfs.NormalizeName(nameOf(l.RelPath))
	if err != nil {
		return true
	}
	occ := p.remoteChild(parent, key)
	return occ == nil || !p.area.in(occ.NodeID)
}

// createRemote uploads a file or creates a folder at l's place on the
// server. forget is the base node it replaces (gone on the server).
func (p *planner) createRemote(l *state.Local, sha, forget string) {
	parent, ok := p.nodeAt[nfc(dirOf(l.RelPath))]
	if !ok || p.remote[parent] == nil {
		p.deferOne()
		return
	}
	if !p.writable(parent) {
		p.problem(ProblemReadOnly, l.RelPath, forget, "a new file in a folder without write access is not uploaded")
		return
	}
	name, key, err := localfs.NormalizeName(nameOf(l.RelPath))
	if err != nil {
		p.problem(ProblemNameInvalid, l.RelPath, forget, err.Error())
		return
	}
	if forget != "" {
		if occ := p.remoteChild(parent, key); occ != nil {
			if p.base[occ.NodeID] != nil && p.willVacate(occ) {
				p.deferOne()
			} else {
				p.renameAside(l, parent, forget)
			}
			return
		}
	}
	op := Op{Path: l.RelPath, ParentID: parent, Name: name, Forget: forget, Include: p.includeFor(parent)}
	if forget != "" {
		op.NodeID = forget
	}
	if l.Kind == state.KindDir {
		op.Kind, op.Dir = OpMkdirRemote, true
	} else {
		st := lstamp(l)
		op.Kind, op.SHA256, op.Size, op.MtimeNS, op.Expect = OpUpload, sha, l.Size, l.MtimeNS, &st
	}
	p.take(dirOf(l.RelPath), name)
	p.add(op)
}

// movesInto: the node leaving the name moves into l itself (a folder
// renamed, then moved into a new folder of its old name). Waiting would
// never end — l needs the name first — so l goes aside.
func (p *planner) movesInto(occ *state.Remote, l *state.Local) bool {
	cl := p.claim[occ.NodeID]
	return cl != nil && below(nfc(cl.RelPath), nfc(l.RelPath))
}

// caseLoser: l shares its name key with another entry of its folder (two
// names a case-sensitive disk keeps apart and the server doesn't). The
// one base knows keeps the name, else the first by bytes.
func (p *planner) caseLoser(l *state.Local, key string) bool {
	same := p.localKids[nfc(dirOf(l.RelPath))][key]
	if len(same) < 2 {
		return false
	}
	winner := same[0]
	for _, o := range same {
		if p.claimed[o] {
			winner = o
			break
		}
		if nfc(o.RelPath) < nfc(winner.RelPath) {
			winner = o
		}
	}
	return winner != l
}

// pair: new on both sides under one name. Equal content (or two folders)
// is linked; otherwise the local one is renamed aside and both are kept.
func (p *planner) pair(l *state.Local, occ *state.Remote, parent string) {
	if p.busyNode(occ.NodeID) {
		p.deferOne()
		return
	}
	// A local folder named like «Доступно мне» pairs with it, but checks
	// no share: each is checked by itself.
	inc := p.area.status(occ.NodeID) == out && occ.NodeID != SharedID
	link := func(sha string) {
		nb := state.Base{NodeID: occ.NodeID, RelPath: l.RelPath, Kind: l.Kind, SHA256: sha, ETag: occ.ETag,
			FileID: l.FileID, Size: l.Size, MtimeNS: l.MtimeNS}
		p.add(Op{Kind: OpSetBase, NodeID: occ.NodeID, Path: l.RelPath, Base: &nb, Include: inc})
	}
	switch {
	case l.Kind == state.KindDir && occ.Kind == state.KindDir:
		link("")
	case l.Kind == state.KindFile && occ.Kind == state.KindFile:
		if !p.stable(l) || l.SHA256Cached == "" {
			p.deferOne()
			return
		}
		if l.SHA256Cached == occ.SHA256 {
			link(occ.SHA256)
			return
		}
		p.renameAside(l, parent, "")
	default:
		p.renameAside(l, parent, "")
	}
}

// newRemote plans downloads and local folders, top down, so a folder
// created in this pass gets its contents in the same pass.
func (p *planner) newRemote() {
	queue := []string{p.s.RootID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, r := range p.kids[id] {
			if !p.area.in(r.NodeID) {
				continue
			}
			if p.base[r.NodeID] != nil {
				if _, ok := p.localPathOf(r.NodeID); ok && r.Kind == state.KindDir {
					queue = append(queue, r.NodeID)
				}
				continue
			}
			if p.fetch(r) {
				queue = append(queue, r.NodeID)
			}
		}
	}
}

// fetch plans one new remote node; true when it is a folder created
// locally in this pass.
func (p *planner) fetch(r *state.Remote) bool {
	if p.busyNode(r.NodeID) {
		p.deferOne()
		return false
	}
	pl, ok := p.localPathOf(r.ParentID)
	if !ok {
		p.deferOne()
		return false
	}
	target := join(pl, r.Name)
	if p.inMoved(target) || p.busyPath(target) {
		p.deferOne()
		return false
	}
	if localfs.Ignored(r.Name) {
		p.problem(ProblemNameIgnored, target, r.NodeID, "the name is one this client ignores on disk")
		return false
	}
	if p.s.Options.WindowsNames {
		if err := localfs.CheckWindowsName(r.Name); err != nil {
			p.problem(ProblemNameWindows, target, r.NodeID, err.Error())
			return false
		}
	}
	if len(p.localKids[nfc(pl)][keyOf(r)]) > 0 {
		// Something on disk has the name: a new entry is paired from the
		// local side, a known one moves away first.
		p.stepAside(pl, r)
		p.deferOne()
		return false
	}
	if p.isTaken(pl, r.Name) {
		p.deferOne()
		return false
	}
	p.take(pl, r.Name)
	if r.Kind == state.KindDir {
		p.add(Op{Kind: OpMkdirLocal, NodeID: r.NodeID, Path: target, Dir: true})
		p.plannedLocal[r.NodeID] = target
		return true
	}
	p.add(Op{Kind: OpDownload, NodeID: r.NodeID, Path: target, ETag: r.ETag, SHA256: r.SHA256, Size: r.Size, MtimeNS: r.MtimeNS})
	return false
}

// stepAside: the known node holding r's name in local folder dir moves
// aside when the server moved it into r itself (a.txt into a new A.TXT on
// a case-insensitive disk): r needs the name first, so waiting would
// never end. Base stays, and the node follows the server's position on
// the next pass.
func (p *planner) stepAside(dir string, r *state.Remote) {
	occ := p.localKids[nfc(dir)][keyOf(r)]
	if len(occ) != 1 {
		return
	}
	id, ok := p.nodeAt[nfc(occ[0].RelPath)]
	if ok && !p.busyNode(id) && p.remoteBelow(id, r.NodeID) && !p.renamed[id] {
		p.renamed[id] = true
		p.renameAside(occ[0], r.ParentID, id)
	}
}

// remoteBelow: id is inside ancestor on the server.
func (p *planner) remoteBelow(id, ancestor string) bool {
	for cur, n := p.remote[id], 0; cur != nil && n < 4096; cur, n = p.remote[cur.ParentID], n+1 {
		if cur.ParentID == ancestor {
			return true
		}
	}
	return false
}

// DefaultOptions are this platform's.
func DefaultOptions(device string) Options {
	return Options{DeviceName: device, WindowsNames: runtime.GOOS == "windows", StableAfter: 3e9}
}
