package nodes

import (
	"net/http"
	"strconv"
	"time"

	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/httpx"
)

type trashItemJSON struct {
	nodeJSON
	TrashedAt time.Time `json:"trashedAt"`
	// OriginalPath is the folder it was deleted from, "" for the root.
	OriginalPath string `json:"originalPath"`
}

// trash moves {id} to the trash (DELETE /nodes/{id}); with If-Match only
// if it still has that ETag, else 412.
func (h *Handlers) trash(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	id, err := h.resolveID(r, u)
	var name string
	if err == nil {
		// Named while it can still be looked up.
		if n, gerr := h.FS.Get(r.Context(), u.ID, id); gerr == nil {
			name = n.Name
		}
		err = h.FS.TrashIfMatch(r.Context(), u.ID, id, ifMatch(r))
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.record(r, audit.NodeTrash, id, name, nil)
	w.WriteHeader(http.StatusNoContent)
}

// listTrash pages through the trash, newest first: ?limit=&cursor=.
func (h *Handlers) listTrash(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	limit := 0
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "limit must be a positive number")
			return
		}
		limit = n
	}
	items, next, err := h.FS.ListTrash(r.Context(), u.ID, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := struct {
		Items      []trashItemJSON `json:"items"`
		NextCursor *string         `json:"nextCursor"`
	}{Items: make([]trashItemJSON, 0, len(items))}
	for _, it := range items {
		resp.Items = append(resp.Items, trashItemJSON{nodeJSON: toJSON(it.Node), TrashedAt: it.TrashedAt, OriginalPath: it.OriginalPath})
	}
	if next != "" {
		resp.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// restore brings a trash item back; the answer is the node as restored,
// renamed if its name was taken meanwhile.
func (h *Handlers) restore(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	n, err := h.FS.Restore(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toJSON(n))
}

// purge deletes a trash item for good.
func (h *Handlers) purge(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	if err := h.FS.Purge(r.Context(), u.ID, r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	// In the trash it has no name to look up: the id says which it was.
	h.Audit.Record(r, audit.Event{Actor: auth.Actor(r.Context()), Action: audit.TrashPurge, TargetID: r.PathValue("id")})
	w.WriteHeader(http.StatusNoContent)
}

// emptyTrash purges the whole trash.
func (h *Handlers) emptyTrash(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	if err := h.FS.EmptyTrash(r.Context(), u.ID); err != nil {
		h.fail(w, r, err)
		return
	}
	h.Audit.Record(r, audit.Event{Actor: auth.Actor(r.Context()), Action: audit.TrashEmpty})
	w.WriteHeader(http.StatusNoContent)
}
