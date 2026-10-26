package nodes

import (
	"net/http"
	"time"

	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/httpx"
)

type versionJSON struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
	// CreatedAt is when this content was saved.
	CreatedAt time.Time `json:"createdAt"`
	// Author saved it; null when unknown.
	Author *authorJSON `json:"author"`
}

type authorJSON struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

func toAuthorJSON(a fs.Author) *authorJSON {
	if a.ID == "" {
		return nil
	}
	return &authorJSON{ID: a.ID, DisplayName: a.Name}
}

// versions lists a file's earlier contents, newest first.
func (h *Handlers) versions(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	list, current, err := h.FS.ListVersions(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := struct {
		Items []versionJSON `json:"items"`
		// CurrentAuthor wrote the file's current content.
		CurrentAuthor *authorJSON `json:"currentAuthor"`
	}{Items: make([]versionJSON, 0, len(list)), CurrentAuthor: toAuthorJSON(current)}
	for _, v := range list {
		resp.Items = append(resp.Items, versionJSON{ID: v.ID, Size: v.Size, CreatedAt: v.CreatedAt, Author: toAuthorJSON(v.Author)})
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// restoreVersion makes a version current again; the answer is the file.
func (h *Handlers) restoreVersion(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	n, err := h.FS.RestoreVersion(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	j := toJSON(n)
	// A file shared on its own: its folder isn't the recipient's to see.
	if n.OwnerID != u.ID {
		path, err := h.FS.Path(r.Context(), u.ID, n.ID)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		j = withPath(n, path).nodeJSON
	}
	httpx.WriteJSON(w, http.StatusOK, j)
}
