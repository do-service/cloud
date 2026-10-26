package sync

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/httpx"
)

// maxLimit bounds the journal rows one page of changes reads; it is also
// the default.
const maxLimit = 1000

type changeJSON struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	// Deleted: the node is gone for the caller — trashed, purged, moved
	// out of what they see. Possibly one they never knew.
	Deleted bool `json:"deleted"`
	// Node is its current state, when not deleted.
	Node *nodeJSON `json:"node,omitempty"`
	// Subtree: a folder whose contents the client may not have — restored
	// from the trash, or moved into what the caller sees — to read with
	// /sync/tree?root=id.
	Subtree bool `json:"subtree,omitempty"`
}

type changesResponse struct {
	Changes []changeJSON `json:"changes"`
	Cursor  string       `json:"cursor"`
	// More: there is more to read right away, from cursor.
	More bool `json:"more"`
	// RootsChanged: what is shared with the caller changed; list the
	// roots again (and read the trees of new ones).
	RootsChanged bool `json:"rootsChanged"`
}

// changes answers what changed since ?cursor=: each node changed once,
// in its current state, or a tombstone. ?limit= bounds the journal rows
// read (default and most 1000). ?wait= (seconds, up to maxWait) makes it
// a long poll: with nothing to report it waits for a change that
// concerns the caller (Hub) and answers then, or with no changes and the
// cursor once the time is up or the server shuts down.
func (h *Handlers) changes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := auth.UserFromContext(ctx)
	q := r.URL.Query()
	from, err := decodeCursor(q.Get("cursor"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "cursor is missing or malformed")
		return
	}
	limit := maxLimit
	if s := q.Get("limit"); s != "" {
		if limit, err = strconv.Atoi(s); err != nil || limit < 1 || limit > maxLimit {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "limit must be 1 to 1000")
			return
		}
	}
	wait := 0
	if s := q.Get("wait"); s != "" {
		if wait, err = strconv.Atoi(s); err != nil || wait < 0 || wait > maxWait {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "wait must be 0 to 55 seconds")
			return
		}
	}
	until := time.Now().Add(time.Duration(wait) * time.Second)

	for {
		resp, view, err := h.read(ctx, u.ID, from, limit)
		switch {
		case errors.Is(err, errExpired):
			httpx.WriteError(w, http.StatusGone, httpx.CodeCursorExpired, "the journal no longer goes back to this cursor: resync in full")
			return
		case err != nil:
			h.fail(w, r, err)
			return
		}
		// Between reads no connection is held: the hub polls for all.
		if len(resp.Changes) > 0 || resp.More || resp.RootsChanged || h.Hub == nil || !time.Now().Before(until) ||
			!h.Hub.wait(ctx, u.ID, view, until) {
			if ctx.Err() == nil {
				httpx.WriteJSON(w, http.StatusOK, resp)
			}
			return
		}
	}
}

var errExpired = errors.New("sync: cursor expired")

// read is one page of changes from the cursor, and the view it was read
// in.
func (h *Handlers) read(ctx context.Context, userID string, from cursor, limit int) (changesResponse, fs.SyncView, error) {
	view, err := h.FS.SyncView(ctx, userID)
	if err != nil {
		return changesResponse{}, view, err
	}
	if expired(from, view) {
		return changesResponse{}, view, errExpired
	}
	next := cursorAt(view)
	resp := changesResponse{Changes: []changeJSON{}, RootsChanged: from.A != next.A || from.R != next.R}

	// Each tree from where the client is, as far as the page goes: their
	// own first, then the others'. A tree new to the cursor starts at its
	// head, its contents come with its tree (rootsChanged says so).
	var entries []entry
	read := func(owner string, after int64, head int64) (int64, error) {
		rows, err := h.FS.Journal(ctx, userID, view, owner, after, limit-len(entries))
		if err != nil {
			return 0, err
		}
		for _, e := range rows {
			entries = append(entries, entry{owner: owner, JournalEntry: e})
		}
		if len(entries) == limit && len(rows) > 0 && rows[len(rows)-1].Seq < head {
			resp.More = true
			return rows[len(rows)-1].Seq, nil
		}
		if len(entries) == limit && len(rows) == 0 && after < head {
			resp.More = true
			return after, nil
		}
		return head, nil
	}
	if next.Own, err = read(userID, from.Own, view.Own.TreeSeq); err != nil {
		return changesResponse{}, view, err
	}
	for _, owner := range view.Owners() {
		after, known := from.S[owner]
		if !known {
			continue
		}
		if next.S[owner], err = read(owner, after, view.Heads[owner].TreeSeq); err != nil {
			return changesResponse{}, view, err
		}
	}

	if resp.Changes, err = h.states(ctx, userID, view, entries); err != nil {
		return changesResponse{}, view, err
	}
	resp.Cursor = next.encode()
	return resp, view, nil
}

// expired reports whether c reaches back past what the journals keep, or
// ahead of them (a database restored from a backup): either way the
// client can't carry on from it.
func expired(c cursor, view fs.SyncView) bool {
	if c.Own < view.Own.MinSeq || c.Own > view.Own.TreeSeq {
		return true
	}
	for owner, seq := range c.S {
		if _, shared := view.Shared[owner]; !shared {
			continue // no longer shared: it drops out of the next cursor
		}
		// No head is all zero: nothing journaled in that tree yet.
		if head := view.Heads[owner]; seq < head.MinSeq || seq > head.TreeSeq {
			return true
		}
	}
	return false
}

type entry struct {
	owner string
	fs.JournalEntry
}

// states turns journal rows into changes: one per node, in its current
// state as the actor sees it now, or a tombstone. Live nodes come first,
// each after its folder when that changed too, then the tombstones: a
// client applying them in order has a node's folder before the node, and
// moves things out of a folder before it deletes the folder.
func (h *Handlers) states(ctx context.Context, actorID string, view fs.SyncView, entries []entry) ([]changeJSON, error) {
	type item struct {
		owner             string
		restored, moved   bool
		node              fs.Node
		live, done, doing bool
	}
	var order []string
	items := map[string]*item{}
	for _, e := range entries {
		it := items[e.NodeID]
		if it == nil {
			it = &item{owner: e.owner}
			items[e.NodeID] = it
			order = append(order, e.NodeID)
		}
		it.restored = it.restored || e.Op == "restore"
		it.moved = it.moved || e.Op == "move"
	}
	for _, id := range order {
		it := items[id]
		n, err := h.FS.Get(ctx, actorID, id)
		switch {
		case errors.Is(err, fs.ErrNotFound):
		case err != nil:
			return nil, err
		default:
			it.node, it.live = n, true
		}
	}

	out := make([]changeJSON, 0, len(order))
	var emit func(id string) error
	emit = func(id string) error {
		it := items[id]
		if it.done || it.doing {
			// doing: states are read one by one, not from one snapshot,
			// so a move in between could make two nodes each other's
			// folder.
			return nil
		}
		it.doing = true
		if p, ok := items[it.node.ParentID]; ok && p.live {
			if err := emit(it.node.ParentID); err != nil {
				return err
			}
		}
		top, err := h.isTop(ctx, actorID, view, it.node)
		if err != nil {
			return err
		}
		j := toJSON(it.node, top)
		// A folder the client may lack the contents of: restored ones,
		// and, in someone else's tree, moved ones — which may come from
		// where the caller couldn't see them.
		subtree := it.node.Kind == fs.KindDir && (it.restored || it.moved && it.owner != actorID)
		out = append(out, changeJSON{ID: id, Owner: it.owner, Node: &j, Subtree: subtree})
		it.done = true
		return nil
	}
	for _, id := range order {
		if items[id].live {
			if err := emit(id); err != nil {
				return nil, err
			}
		}
	}
	for _, id := range order {
		if it := items[id]; !it.live {
			out = append(out, changeJSON{ID: id, Owner: it.owner, Deleted: true})
		}
	}
	return out, nil
}
