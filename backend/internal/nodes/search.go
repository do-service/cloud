package nodes

import (
	"net/http"
	"strconv"

	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/httpx"
)

type searchResultJSON struct {
	nodeJSON
	// Path is the folder the node is in, "" for the root.
	Path string `json:"path"`
	// ParentID is that folder, so a result can link to it. Listings leave
	// it out: there the folder is the one being listed.
	ParentID string `json:"parentId"`
}

// search finds nodes by name: ?q=&limit=&cursor=.
func (h *Handlers) search(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	q := r.URL.Query()
	limit := 0
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "limit must be a positive number")
			return
		}
		limit = n
	}
	items, next, err := h.FS.Search(r.Context(), u.ID, q.Get("q"), limit, q.Get("cursor"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := struct {
		Items      []searchResultJSON `json:"items"`
		NextCursor *string            `json:"nextCursor"`
	}{Items: make([]searchResultJSON, 0, len(items))}
	for _, it := range items {
		resp.Items = append(resp.Items, searchResultJSON{nodeJSON: toJSON(it.Node), Path: it.Path, ParentID: it.ParentID})
	}
	if next != "" {
		resp.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}
