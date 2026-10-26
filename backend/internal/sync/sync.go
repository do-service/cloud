// Package sync serves the API of sync clients (E11-2, docs/desktop-client.md):
// /api/v1/sync/info, roots, tree and changes. A client lists its roots,
// reads each tree, then follows the change journal from the cursor it got.
// Like nodes, it only translates HTTP to fs and share: access checks, the
// journal and every rule about the tree live there.
package sync

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/httpx"
	"github.com/do-service/cloud/backend/internal/share"
	"github.com/do-service/cloud/backend/internal/version"
)

const (
	// apiVersion changes with an incompatible change to this API.
	apiVersion = 1
	// minClientVersion is the oldest client the server works with; none
	// is too old yet (E15-4 makes it a setting).
	minClientVersion = "0.0.0"
	// maxWait bounds a long poll of changes, below the 60 s idle timeout
	// proxies commonly have.
	maxWait = 55
)

type Handlers struct {
	FS     *fs.Service
	Shares *share.Service
	// Hub wakes long polls; nil answers ?wait= at once.
	Hub *Hub
	// JournalRetention is how long the journal keeps rows
	// (SYNC_JOURNAL_RETENTION), so how long a client may stay offline
	// without a full resync; 0 is for good.
	JournalRetention time.Duration
	// SignedIn wraps a route so it runs only for a signed-in user, found
	// in the request context (auth.Middleware + auth.RequireUser).
	SignedIn func(http.Handler) http.Handler
}

func (h *Handlers) Register(mux *http.ServeMux) {
	// Before signing in: a client checks it can talk to the server.
	mux.HandleFunc("GET /api/v1/sync/info", h.info)
	mux.Handle("GET /api/v1/sync/roots", h.SignedIn(http.HandlerFunc(h.roots)))
	mux.Handle("GET /api/v1/sync/tree", h.SignedIn(http.HandlerFunc(h.tree)))
	mux.Handle("GET /api/v1/sync/changes", h.SignedIn(http.HandlerFunc(h.changes)))
}

type infoJSON struct {
	APIVersion       int    `json:"apiVersion"`
	ServerVersion    string `json:"serverVersion"`
	MinClientVersion string `json:"minClientVersion"`
	// JournalRetention and MaxWait are in seconds.
	JournalRetention int64 `json:"journalRetention"`
	MaxWait          int   `json:"maxWait"`
}

func (h *Handlers) info(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, infoJSON{
		APIVersion: apiVersion, ServerVersion: version.Version, MinClientVersion: minClientVersion,
		JournalRetention: int64(h.JournalRetention / time.Second), MaxWait: maxWait,
	})
}

// nodeJSON is a node as sync reports it: with its parent, to place it,
// and its content's SHA-256, to tell an edit from a touch and to skip
// downloads of what the client has.
type nodeJSON struct {
	ID string `json:"id"`
	// ParentID is null at the top of what the caller sees: their root, or
	// a node shared with them whose folder isn't.
	ParentID *string   `json:"parentId"`
	Name     string    `json:"name"`
	Kind     fs.Kind   `json:"kind"`
	Size     int64     `json:"size"`
	ETag     string    `json:"etag"`
	SHA256   string    `json:"sha256,omitempty"`
	MTime    time.Time `json:"mtime"`
	// Permissions are the caller's rights on it, named as in the nodes API.
	Permissions []string `json:"permissions"`
}

func toJSON(n fs.Node, top bool) nodeJSON {
	j := nodeJSON{ID: n.ID, Name: n.Name, Kind: n.Kind, Size: n.Size, ETag: n.ETag, MTime: n.MTime, Permissions: permNames(n.Perms)}
	if n.ParentID != "" && !top {
		j.ParentID = &n.ParentID
	}
	if len(n.ContentHash) > 0 {
		j.SHA256 = hex.EncodeToString(n.ContentHash)
	}
	return j
}

// permNames spells a rights mask out, with the same names as the nodes
// API, so a client reads both alike.
func permNames(p access.Perm) []string {
	names := []string{}
	for _, r := range []struct {
		perm access.Perm
		name string
	}{{access.Read, "read"}, {access.Write, "write"}, {access.Reshare, "reshare"}} {
		if p.Has(r.perm) {
			names = append(names, r.name)
		}
	}
	return names
}

// isTop reports whether n is at the top of what actorID sees: their root,
// or a node of someone else's shared with them whose folder they can't
// see. Its parent's id is then none of their business, and no place for
// the client to put it.
func (h *Handlers) isTop(ctx context.Context, actorID string, view fs.SyncView, n fs.Node) (bool, error) {
	if n.ParentID == "" {
		return true, nil
	}
	if n.OwnerID == actorID {
		return false, nil
	}
	if _, shared := view.Shared[n.OwnerID][n.ID]; !shared {
		// Nothing is shared right there, so its rights come from above:
		// the folder is visible too.
		return false, nil
	}
	_, err := h.FS.Get(ctx, actorID, n.ParentID)
	if errors.Is(err, fs.ErrNotFound) {
		return true, nil
	}
	return false, err
}

type ownerJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type rootJSON struct {
	ID string `json:"id"`
	// Kind is "own" for the caller's root, "shared" for what others
	// shared with them.
	Kind string `json:"kind"`
	// Name is "" for the caller's root, which the client names itself; a
	// shared one is named as in WebDAV's «Доступно мне»: «Имя (Владелец)»
	// where names repeat, then «(2)» and on.
	Name        string    `json:"name"`
	Owner       ownerJSON `json:"owner"`
	Permissions []string  `json:"permissions"`
	ETag        string    `json:"etag"`
}

type rootsResponse struct {
	Roots  []rootJSON `json:"roots"`
	Cursor string     `json:"cursor"`
}

// roots lists where the caller's sync starts, with a cursor taken before
// the listing: changes from it cover the trees the client reads after.
func (h *Handlers) roots(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := auth.UserFromContext(ctx)
	view, err := h.FS.SyncView(ctx, u.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	root, err := h.FS.Root(ctx, u.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items, err := h.Shares.SharedWithMe(ctx, u.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	name := u.DisplayName
	if name == "" {
		name = u.Username
	}
	resp := rootsResponse{
		Roots: []rootJSON{{
			ID: root.ID, Kind: "own", Owner: ownerJSON{ID: u.ID, Name: name},
			Permissions: permNames(access.All), ETag: root.ETag,
		}},
		Cursor: cursorAt(view).encode(),
	}
	for _, e := range share.Entries(items) {
		resp.Roots = append(resp.Roots, rootJSON{
			ID: e.Item.ID, Kind: "shared", Name: e.Name, Owner: ownerJSON{ID: e.Item.Owner.ID, Name: e.Item.Owner.Name},
			Permissions: permNames(e.Item.Perms), ETag: e.Item.ETag,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// fail answers with the API error for an fs error.
func (h *Handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, fs.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such file or folder")
	case errors.Is(err, fs.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "your rights on this don't allow it")
	default:
		slog.ErrorContext(r.Context(), "sync: request failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
	}
}
