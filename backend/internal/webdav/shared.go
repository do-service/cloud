package webdav

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/share"
)

// What others shared with the user (E7-4) is a virtual folder in their
// root, holding each topmost shared node — the web UI's "Доступно мне".
// Below an entry the paths are ordinary: fs resolves them and checks the
// rights the share grants. The folder and its entries themselves can't be
// created, removed or renamed: leaving a share is done in the web UI, so a
// sync client deleting its copy can't drop the access by accident.

// sharedFolderName is the virtual folder's name; a real folder of the
// user's by that name keeps it, and the virtual one takes "(2)" and on.
const sharedFolderName = "Доступно мне"

// sharedEntry is a shared node as the virtual folder lists it.
type sharedEntry struct {
	node fs.Node
	name string
}

// sharedView is what a request learned about the virtual folder, worked
// out once per request: its name, its entries.
type sharedView struct {
	nameOnce sync.Once
	name     string
	nameErr  error

	entriesOnce sync.Once
	entries     []sharedEntry
	entriesErr  error
}

func nameKey(name string) string {
	if _, key, err := fs.NormalizeName(name); err == nil {
		return key
	}
	return name
}

// mayBeShared tells cheaply whether a root name could be the virtual
// folder's, before its actual name is worked out.
func mayBeShared(name string) bool {
	return strings.HasPrefix(nameKey(name), nameKey(sharedFolderName))
}

// sharedName is the virtual folder's name: the first of "Доступно мне",
// "Доступно мне (2)", … that no real folder in the root has.
func (h *Handler) sharedName(ctx context.Context, t target) (string, error) {
	v := t.view
	v.nameOnce.Do(func() {
		for i := 1; i <= 100; i++ {
			name := sharedFolderName
			if i > 1 {
				name = fmt.Sprintf("%s (%d)", sharedFolderName, i)
			}
			_, err := h.files.Lookup(ctx, t.user.ID, t.root.ID, []string{name})
			if errors.Is(err, fs.ErrNotFound) {
				v.name = name
				return
			}
			if err != nil {
				v.nameErr = err
				return
			}
		}
		v.nameErr = fs.ErrNameConflict
	})
	return v.name, v.nameErr
}

// sharedEntries lists the virtual folder, named as share.Entries has it.
func (h *Handler) sharedEntries(ctx context.Context, t target) ([]sharedEntry, error) {
	v := t.view
	v.entriesOnce.Do(func() {
		items, err := h.shares.SharedWithMe(ctx, t.user.ID)
		if err != nil {
			v.entriesErr = err
			return
		}
		for _, e := range share.Entries(items) {
			v.entries = append(v.entries, sharedEntry{node: e.Item.Node, name: e.Name})
		}
	})
	return v.entries, v.entriesErr
}

// sharedFolder is the virtual folder as a node. It has no id; its ETag
// changes whenever any entry's does, so sync clients look inside.
func (h *Handler) sharedFolder(ctx context.Context, t target) (fs.Node, error) {
	name, err := h.sharedName(ctx, t)
	if err != nil {
		return fs.Node{}, err
	}
	entries, err := h.sharedEntries(ctx, t)
	if err != nil {
		return fs.Node{}, err
	}
	n := fs.Node{Name: name, Kind: fs.KindDir, OwnerID: t.user.ID, MTime: time.Unix(0, 0)}
	sum := sha256.New()
	for _, e := range entries {
		n.Size += e.node.Size
		if e.node.MTime.After(n.MTime) {
			n.MTime = e.node.MTime
		}
		fmt.Fprintf(sum, "%s\x00%s\x00%s\n", e.node.ID, e.node.ETag, e.name)
	}
	n.ETag = hex.EncodeToString(sum.Sum(nil))[:32]
	return n, nil
}

// sharedLookup resolves names (the virtual folder's first) to a node.
func (h *Handler) sharedLookup(ctx context.Context, t target, names []string) (fs.Node, error) {
	if len(names) == 1 {
		return h.sharedFolder(ctx, t)
	}
	entries, err := h.sharedEntries(ctx, t)
	if err != nil {
		return fs.Node{}, err
	}
	i := slices.IndexFunc(entries, func(e sharedEntry) bool { return nameKey(e.name) == nameKey(names[1]) })
	if i < 0 {
		return fs.Node{}, fs.ErrNotFound
	}
	if len(names) == 2 {
		return entries[i].node, nil
	}
	return h.files.Lookup(ctx, t.user.ID, entries[i].node.ID, names[2:])
}

// writeSharedFolder adds the virtual folder to a root listing, when there
// is anything shared to show.
func (h *Handler) writeSharedFolder(ctx context.Context, mw *multistatusWriter, p *props, pf propfind, t target) error {
	entries, err := h.sharedEntries(ctx, t)
	if err != nil || len(entries) == 0 {
		return err
	}
	n, err := h.sharedFolder(ctx, t)
	if err != nil {
		return err
	}
	names := []string{n.Name}
	return mw.write(t.href(names, true), p.find(pf, n, names))
}
