package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// Shared folders (docs/desktop-client.md, «Общие папки»): what others
// share with the user is mirrored under a folder of the sync folder's
// root, «Доступно мне», each share by its /sync/roots name («Имя
// (Владелец)» where names repeat). The folder is no node of the server:
// the mirror has it as SharedID, read-only, so the planner never uploads
// into it, and each shared root's row hangs below it under its root
// name. Selecting "/" leaves the shares out (area); they are checked one
// by one, or all with «Доступно мне».

const (
	// SharedID is the mirror's id of «Доступно мне»; no server id looks
	// like it.
	SharedID = "~shared"
	// SharedName is its name, as WebDAV's and the web UI's.
	SharedName = "Доступно мне"
)

// sharedFolder is the mirror row of «Доступно мне» in the own root
// ownID.
func sharedFolder(ownID, name string) state.Remote {
	return state.Remote{NodeID: SharedID, ParentID: ownID, Name: name, NameKey: localfs.NameKey(name),
		Kind: state.KindDir, Perms: state.PermRead}
}

// placeRoot puts a shared root's row into «Доступно мне» under its root
// name: the server reports it at the top (no parent) with its own name.
func placeRoot(n *state.Remote, name string) {
	n.ParentID, n.Name, n.NameKey = SharedID, name, localfs.NameKey(name)
}

// fixSharedFolder keeps «Доступно мне» in the mirror under a name no own
// folder of the root has: the first free of "Доступно мне", "Доступно
// мне (2)", …, as WebDAV names it.
func fixSharedFolder(tx *state.Tx, ownID string) error {
	if ownID == "" {
		return nil
	}
	kids, err := tx.RemoteChildren(ownID)
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	for _, k := range kids {
		if k.NodeID != SharedID {
			taken[k.NameKey] = true
		}
	}
	name := SharedName
	for i := 2; taken[localfs.NameKey(name)]; i++ {
		name = fmt.Sprintf("%s (%d)", SharedName, i)
	}
	cur, err := tx.Remote(SharedID)
	if err == nil && cur.ParentID == ownID && cur.Name == name {
		return nil
	}
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return err
	}
	return tx.PutRemote(sharedFolder(ownID, name))
}

// sharedTrees lists the trees of the shared roots, placed in «Доступно
// мне». A root gone meanwhile (revoked again) is left out: the next
// roots listing drops it.
func (e *Engine) sharedTrees(ctx context.Context, roots []api.Root) (map[string][]state.Remote, error) {
	out := map[string][]state.Remote{}
	for _, root := range roots {
		if root.Kind != string(state.RootShared) {
			continue
		}
		var nodes []state.Remote
		_, err := e.r.Tree(ctx, root.ID, func(n api.Node) error {
			r := toRemote(n)
			if r.NodeID == root.ID {
				placeRoot(&r, root.Name)
			}
			nodes = append(nodes, r)
			return nil
		})
		if errors.Is(err, api.ErrNotFound) || errors.Is(err, api.ErrForbidden) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("engine: list the shared %q: %w", root.Name, err)
		}
		out[root.ID] = nodes
	}
	return out, nil
}

// replaceShared applies a fresh roots listing to the mirror: a share no
// longer listed leaves it with everything below it (the server sends no
// tombstones for a revoked share: the owner just drops out of the
// cursor), a listed one gets its tree anew (rights may have changed
// anywhere in it), or, without a fresh tree, its root name.
func replaceShared(tx *state.Tx, roots []api.Root, trees map[string][]state.Remote) error {
	old, err := tx.Roots()
	if err != nil {
		return err
	}
	listed := map[string]bool{}
	for _, r := range roots {
		if r.Kind == string(state.RootShared) {
			listed[r.ID] = true
		}
	}
	for _, r := range old {
		if r.Kind == state.RootShared && !listed[r.NodeID] {
			if err := deleteRemoteTree(tx, r.NodeID); err != nil {
				return err
			}
		}
	}
	for _, root := range roots {
		if root.Kind != string(state.RootShared) {
			continue
		}
		nodes, ok := trees[root.ID]
		if !ok {
			n, err := tx.Remote(root.ID)
			if errors.Is(err, state.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			placeRoot(&n, root.Name)
			if err := tx.PutRemote(n); err != nil {
				return err
			}
			continue
		}
		if err := deleteRemoteTree(tx, root.ID); err != nil {
			return err
		}
		for _, n := range nodes {
			if err := tx.PutRemote(n); err != nil {
				return err
			}
		}
	}
	return tx.ReplaceRoots(toRoots(roots))
}

// Share is something shared with the user, as the CLI and the app list
// them.
type Share struct {
	NodeID string
	// Name is its folder's name in «Доступно мне»; Path its cloud path
	// for Select ("/Доступно мне/Имя").
	Name, Path string
	Owner      string
	Kind       state.Kind
	Perms      state.Perm
	// Selected: synced (checked itself, or with «Доступно мне»).
	Selected bool
}

// Shares lists what is shared with the user, by name, from the state
// database alone: no engine or network needed.
func Shares(ctx context.Context, db *state.DB) ([]Share, error) {
	var out []Share
	err := db.View(ctx, func(tx *state.Tx) error {
		roots, err := tx.Roots()
		if err != nil {
			return err
		}
		remote := map[string]*state.Remote{}
		if err := tx.EachRemote(func(r state.Remote) error {
			rr := r
			remote[r.NodeID] = &rr
			return nil
		}); err != nil {
			return err
		}
		rules, err := tx.Rules()
		if err != nil {
			return err
		}
		var ownID string
		for _, r := range roots {
			if r.Kind == state.RootOwn {
				ownID = r.NodeID
			}
		}
		folder := SharedName
		if f := remote[SharedID]; f != nil {
			folder = f.Name
		}
		a := newArea(ownID, remote, rules)
		for _, r := range roots {
			if r.Kind != state.RootShared {
				continue
			}
			s := Share{NodeID: r.NodeID, Name: r.Name, Path: "/" + folder + "/" + r.Name, Owner: r.OwnerName, Perms: r.Perms}
			if n := remote[r.NodeID]; n != nil {
				s.Kind, s.Perms = n.Kind, n.Perms
				s.Selected = a.status(r.NodeID) == included
			}
			out = append(out, s)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}
