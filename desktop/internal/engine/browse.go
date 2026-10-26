package engine

import (
	"context"
	"errors"
	"strings"

	"github.com/do-service/cloud/desktop/internal/state"
)

// What the window's cloud browser shows next to each item
// (docs/desktop-client.md, «Интерфейс»): its checkbox and its sync
// status, from the mirror, the rules, base, the journal and the problems.

// ErrNotReady: the mirror hasn't been listed yet (Prepare hasn't
// succeeded), so nothing can be told about the cloud's items.
var ErrNotReady = errors.New("engine: the cloud hasn't been listed yet")

// Check is an item's checkbox.
type Check string

const (
	CheckOff Check = "off"
	// CheckPartial: a folder with something checked below it, or a
	// checked one with something unchecked below it.
	CheckPartial Check = "partial"
	CheckOn      Check = "on"
)

// SyncState is how an item on disk stands; SyncNone for one that isn't.
type SyncState string

const (
	SyncNone    SyncState = ""
	SyncDone    SyncState = "synced"
	SyncPending SyncState = "pending"
	// SyncError: a problem with it, or for a folder with something below.
	SyncError SyncState = "error"
)

// Mark is what the window shows for an item.
type Mark struct {
	Check Check     `json:"check"`
	Sync  SyncState `json:"sync"`
}

// Marks tells the marks of the mirror's nodes ids; ids the mirror doesn't
// have (gone meanwhile) are left out. A folder's status sums up its
// subtree's: an error below it is its error, a transfer below it makes
// it pending. It reads the whole mirror, like Select: the window asks for
// one folder's items at a time, not per item.
func (e *Engine) Marks(ctx context.Context, ids []string) (map[string]Mark, error) {
	if e.ownRoot() == "" {
		return nil, ErrNotReady
	}
	t, rules, err := e.loadTree(ctx)
	if err != nil {
		return nil, err
	}
	base := map[string]state.Base{}
	var ops []state.Op
	var probs []state.Problem
	if err := e.db.View(ctx, func(tx *state.Tx) error {
		if err := tx.EachBase(func(b state.Base) error { base[b.NodeID] = b; return nil }); err != nil {
			return err
		}
		var err error
		if ops, err = tx.Ops(); err != nil {
			return err
		}
		probs, err = tx.Problems()
		return err
	}); err != nil {
		return nil, err
	}
	a := newArea(t.rootID, t.byID, rules)

	// Folders with an unchecked rule below them: a checked one of them is
	// only partly synced.
	excludedBelow := map[string]bool{}
	for id, mode := range rules {
		if mode != state.Exclude || t.byID[id] == nil {
			continue
		}
		for p := a.parentOf(id); p != "" && !excludedBelow[p]; p = a.parentOf(p) {
			excludedBelow[p] = true
		}
	}

	// Statuses by local path ("/"-separated, relative to the sync
	// folder, as ops and problems have them), each raised up the chain.
	worst := map[string]SyncState{}
	raise := func(rel string, s SyncState) {
		for {
			if rank(s) <= rank(worst[rel]) {
				// Its folders have it already.
				return
			}
			worst[rel] = s
			if rel == "" {
				return
			}
			i := strings.LastIndexByte(rel, '/')
			if i < 0 {
				rel = ""
			} else {
				rel = rel[:i]
			}
		}
	}
	rel := func(id string) (string, bool) {
		p, ok := t.path(id)
		return strings.TrimPrefix(p, "/"), ok
	}
	for id, r := range t.byID {
		if r.Kind != state.KindFile || a.status(id) != included {
			continue
		}
		if b, ok := base[id]; !ok || b.ETag != r.ETag {
			if p, ok := rel(id); ok {
				raise(p, SyncPending)
			}
		}
	}
	for _, o := range ops {
		raise(o.Path, SyncPending)
	}
	for _, p := range probs {
		// Problems of the whole connection (offline, the folder's marker)
		// are the connection's status, not its items'.
		if p.RelPath == "" || p.Kind == ProblemOffline {
			continue
		}
		raise(p.RelPath, SyncError)
	}

	out := make(map[string]Mark, len(ids))
	for _, id := range ids {
		if t.byID[id] == nil {
			continue
		}
		var m Mark
		switch a.status(id) {
		case included:
			m.Check = CheckOn
			if excludedBelow[id] {
				m.Check = CheckPartial
			}
		case container:
			m.Check = CheckPartial
		default:
			m.Check = CheckOff
		}
		if m.Check != CheckOff {
			m.Sync = SyncDone
			if p, ok := rel(id); ok && worst[p] != SyncNone {
				m.Sync = worst[p]
			}
		}
		out[id] = m
	}
	return out, nil
}

func rank(s SyncState) int {
	switch s {
	case SyncError:
		return 3
	case SyncPending:
		return 2
	case SyncDone:
		return 1
	}
	return 0
}

// Usage is the window's footer: what the checked items take in the
// cloud, against the free space on the sync folder's disk.
type Usage struct {
	Selected int64 `json:"selected"`
	// Free is -1 where the OS doesn't tell.
	Free int64 `json:"free"`
}

func (e *Engine) Usage(ctx context.Context) (Usage, error) {
	if e.ownRoot() == "" {
		return Usage{}, ErrNotReady
	}
	t, rules, err := e.loadTree(ctx)
	if err != nil {
		return Usage{}, err
	}
	a := newArea(t.rootID, t.byID, rules)
	u := Usage{Free: -1}
	for id, r := range t.byID {
		if r.Kind == state.KindFile && a.status(id) == included {
			u.Selected += r.Size
		}
	}
	if free, ok := freeSpaceOf(e.cfg.Root); ok {
		u.Free = free
	}
	return u, nil
}
