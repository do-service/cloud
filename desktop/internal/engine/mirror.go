package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// The mirror of the server's tree: the user's whole own tree and every
// tree shared with them (under «Доступно мне», shared.go), though only
// the selected area is synced. Reason: the change journal covers the
// whole tree anyway, selecting a folder must resolve its path and needs
// no new listing, a folder moved into the selected area needs no
// /sync/tree of its own, and the planner checks names against what the
// server really has (an unchecked file named like a new local one).

// pullRemote brings the mirror up to date: the first time (or after 410)
// with a full listing, then page by page of /sync/changes, each page and
// its cursor in one transaction. It never long-polls: a pass doesn't
// wait for the server. Right after an idle long poll that brought the
// mirror up to date (pollRemote), it has nothing to ask.
func (e *Engine) pullRemote(ctx context.Context) error {
	e.pullMu.Lock()
	defer e.pullMu.Unlock()
	if e.fresh {
		e.fresh = false
		return nil
	}
	var cursor string
	if err := e.db.View(ctx, func(tx *state.Tx) error {
		a, err := tx.Account()
		cursor = a.Cursor
		return err
	}); err != nil {
		return err
	}
	if cursor == "" || e.rootID == "" {
		return e.resync(ctx)
	}
	for {
		ch, err := e.r.Changes(ctx, cursor, 0, 0)
		if errors.Is(err, api.ErrCursorExpired) {
			e.log.Info("sync: the change journal no longer reaches the cursor; listing the tree again")
			return e.resync(ctx)
		}
		if err != nil {
			return err
		}
		if err := e.applyChanges(ctx, ch); err != nil {
			return err
		}
		cursor = ch.Cursor
		if !ch.More {
			return nil
		}
	}
}

// infoer is a Remote that reports the server's limits (api.Client.Info):
// its MaxWait caps the long poll.
type infoer interface {
	Info(ctx context.Context) (api.Info, error)
}

// longPollWait is Config.Wait capped by the server's maxWait, asked once
// (again after a failure). A server that says nothing (an old one) gets
// Config.Wait: it ignores wait anyway.
func (e *Engine) longPollWait(ctx context.Context) time.Duration {
	if e.cfg.Wait < 0 {
		return 0
	}
	if e.maxWait < 0 {
		return e.cfg.Wait
	}
	if e.maxWait > 0 {
		return min(e.cfg.Wait, e.maxWait)
	}
	in, ok := e.r.(infoer)
	if !ok {
		e.maxWait = -1
		return e.cfg.Wait
	}
	ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := in.Info(ictx)
	switch {
	case err != nil && info.APIVersion == 0:
		return min(e.cfg.Wait, e.cfg.PollInterval)
	case info.MaxWait > 0:
		e.maxWait = info.MaxWait
		return min(e.cfg.Wait, e.maxWait)
	}
	e.maxWait = -1
	return e.cfg.Wait
}

// pollRemote is the idle long poll: /sync/changes with a wait, the
// server answering when something changes (or at once, if it ignores
// wait). What it brings goes into the mirror, unless a result recorded
// meanwhile may be newer than the answer (mirrorGen): then the pass pulls
// again. It reports whether the pass has something new; a failure is
// left to the pass to meet.
func (e *Engine) pollRemote(ctx context.Context) bool {
	wait := e.longPollWait(ctx)
	e.pullMu.Lock()
	gen := e.mirrorGen
	e.pullMu.Unlock()
	var cursor string
	if err := e.db.View(ctx, func(tx *state.Tx) error {
		a, err := tx.Account()
		cursor = a.Cursor
		return err
	}); err != nil || cursor == "" || e.rootID == "" {
		return true
	}
	ch, err := e.r.Changes(ctx, cursor, 0, wait)
	if err != nil {
		if errors.Is(err, api.ErrCursorExpired) {
			return true
		}
		if ctx.Err() == nil {
			e.log.Debug("sync: long poll failed", "err", err)
		}
		return false
	}
	empty := len(ch.Changes) == 0 && !ch.More && !ch.RootsChanged
	e.pullMu.Lock()
	defer e.pullMu.Unlock()
	if gen != e.mirrorGen {
		return true
	}
	var now string
	if err := e.db.View(ctx, func(tx *state.Tx) error {
		a, err := tx.Account()
		now = a.Cursor
		return err
	}); err != nil || now != cursor {
		return true
	}
	if empty && ch.Cursor == cursor {
		e.fresh = true
		return false
	}
	if err := e.applyChanges(ctx, ch); err != nil {
		return true
	}
	e.fresh = !ch.More
	return true
}

// resync replaces the mirror with a fresh listing. Base stays: the
// planner compares by SHA-256, so what the client has isn't downloaded
// again.
func (e *Engine) resync(ctx context.Context) error {
	roots, err := e.r.Roots(ctx)
	if err != nil {
		return err
	}
	var own *api.Root
	for i := range roots.Roots {
		if roots.Roots[i].Kind == string(state.RootOwn) {
			own = &roots.Roots[i]
		}
	}
	if own == nil {
		return errors.New("engine: the server listed no own root")
	}
	var nodes []state.Remote
	cursor, err := e.r.Tree(ctx, own.ID, func(n api.Node) error {
		nodes = append(nodes, toRemote(n))
		return nil
	})
	if err != nil {
		return fmt.Errorf("engine: list the tree: %w", err)
	}
	shared, err := e.sharedTrees(ctx, roots.Roots)
	if err != nil {
		return err
	}
	if roots.Cursor != "" {
		// From before the listing: changes from it cover every tree read
		// after, and a share made meanwhile still says rootsChanged.
		cursor = roots.Cursor
	}
	err = e.db.Update(ctx, func(tx *state.Tx) error {
		if err := tx.ClearRemote(); err != nil {
			return err
		}
		for _, n := range nodes {
			if err := tx.PutRemote(n); err != nil {
				return err
			}
		}
		if err := replaceShared(tx, roots.Roots, shared); err != nil {
			return err
		}
		if err := fixSharedFolder(tx, own.ID); err != nil {
			return err
		}
		return tx.SetCursor(cursor)
	})
	if err == nil {
		e.rootID = own.ID
	}
	return err
}

func (e *Engine) applyChanges(ctx context.Context, ch api.Changes) error {
	roots := map[string]string{} // shared root → its name in «Доступно мне»
	mirrored := map[string]bool{}
	if err := e.db.View(ctx, func(tx *state.Tx) error {
		known, err := tx.Roots()
		for _, r := range known {
			if r.Kind != state.RootShared {
				continue
			}
			roots[r.NodeID] = r.Name
			if _, err := tx.Remote(r.NodeID); err == nil {
				mirrored[r.NodeID] = true
			} else if !errors.Is(err, state.ErrNotFound) {
				return err
			}
		}
		return err
	}); err != nil {
		return err
	}
	// Subtrees are listed before the transaction: no request holds it.
	subtrees := map[string][]state.Remote{}
	// A listed share without its tree (one that failed to list, or a
	// state from before shares were synced) is listed again.
	relist := ch.RootsChanged || len(mirrored) < len(roots)
	for _, c := range ch.Changes {
		if _, ok := roots[c.ID]; ok {
			// A shared root renamed (or moved) by its owner: its name in
			// «Доступно мне» is the listing's.
			relist = true
		}
		if !c.Subtree || c.Deleted {
			continue
		}
		var nodes []state.Remote
		_, err := e.r.Tree(ctx, c.ID, func(n api.Node) error {
			nodes = append(nodes, toRemote(n))
			return nil
		})
		if errors.Is(err, api.ErrNotFound) {
			continue // gone again; its tombstone comes
		}
		if err != nil {
			return err
		}
		subtrees[c.ID] = nodes
	}
	var listing []api.Root
	var trees map[string][]state.Remote
	if relist {
		r, err := e.r.Roots(ctx)
		if err != nil {
			return err
		}
		listing = r.Roots
		// Trees are read after the page, so they are newer than its
		// states. With rootsChanged every shared one: what is shared, and
		// with which rights, changed somewhere in them; else only a root
		// the mirror lacks.
		var read []api.Root
		for _, root := range listing {
			if root.Kind != string(state.RootShared) {
				continue
			}
			if ch.RootsChanged || !mirrored[root.ID] {
				read = append(read, root)
			}
			roots[root.ID] = root.Name
		}
		if trees, err = e.sharedTrees(ctx, read); err != nil {
			return err
		}
	}
	return e.db.Update(ctx, func(tx *state.Tx) error {
		for _, c := range ch.Changes {
			if c.Deleted || c.Node == nil {
				if err := deleteRemoteTree(tx, c.ID); err != nil {
					return err
				}
				continue
			}
			n := toRemote(*c.Node)
			switch name, shared := roots[c.ID]; {
			case c.ID == e.rootID:
				n.ParentID = ""
			case shared:
				placeRoot(&n, name)
			case n.ParentID == "":
				// At the top of what the user sees but no root of theirs
				// (shared after the listing): the next one places it.
				continue
			}
			if err := tx.PutRemote(n); err != nil {
				return err
			}
			for _, s := range subtrees[c.ID] {
				if s.NodeID == c.ID {
					continue
				}
				if err := tx.PutRemote(s); err != nil {
					return err
				}
			}
		}
		if listing != nil {
			if err := replaceShared(tx, listing, trees); err != nil {
				return err
			}
		}
		if err := fixSharedFolder(tx, e.rootID); err != nil {
			return err
		}
		return tx.SetCursor(ch.Cursor)
	})
}

// deleteRemoteTree forgets a node and what the mirror has below it: the
// server sends one tombstone for a trashed folder.
func deleteRemoteTree(tx *state.Tx, id string) error {
	kids, err := tx.RemoteChildren(id)
	if err != nil {
		return err
	}
	for _, k := range kids {
		if err := deleteRemoteTree(tx, k.NodeID); err != nil {
			return err
		}
	}
	return tx.DeleteRemote(id)
}

func toRemote(n api.Node) state.Remote {
	return state.Remote{
		NodeID: n.ID, ParentID: n.ParentID, Name: n.Name, NameKey: localfs.NameKey(n.Name),
		Kind: state.Kind(n.Kind), Size: n.Size, ETag: n.ETag, SHA256: n.SHA256,
		MtimeNS: n.MTime.UnixNano(), Perms: state.ParsePerms(n.Permissions),
	}
}

func toRoots(rs []api.Root) []state.Root {
	out := make([]state.Root, 0, len(rs))
	for _, r := range rs {
		out = append(out, state.Root{NodeID: r.ID, Kind: state.RootKind(r.Kind), Name: r.Name, OwnerID: r.Owner.ID,
			OwnerName: r.Owner.Name, Perms: state.ParsePerms(r.Permissions), ETag: r.ETag})
	}
	return out
}
