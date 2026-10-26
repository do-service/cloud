package nodes

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/httpx"
	"github.com/do-service/cloud/backend/internal/preview"
	"github.com/do-service/cloud/backend/internal/share"
)

// preview serves a file's preview: ?size=s (lists) or l (viewing).
func (h *Handlers) preview(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	h.servePreview(w, r, u.ID, r.PathValue("id"))
}

// publicPreview is preview through a public link: what it may read.
func (h *Handlers) publicPreview(w http.ResponseWriter, r *http.Request) {
	p := linkFrom(r)
	if p.Mode != share.ModeView && p.Mode != share.ModeDownload {
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "this link only takes uploads")
		return
	}
	h.servePreview(w, r, p.Principal(), linkNodeID(r, p))
}

func (h *Handlers) servePreview(w http.ResponseWriter, r *http.Request, actorID, id string) {
	size := preview.Size(r.URL.Query().Get("size"))
	if size == "" {
		size = preview.Small
	}
	if !size.Valid() {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "size is s or l")
		return
	}
	rc, n, err := h.Preview.Open(r.Context(), actorID, id, size)
	if errors.Is(err, preview.ErrNone) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no preview for this file")
		return
	}
	if err != nil {
		if !errors.Is(err, r.Context().Err()) {
			h.fail(w, r, err)
		}
		return
	}
	defer rc.Close()
	hdr := w.Header()
	hdr.Set("Content-Type", "image/webp")
	// The UI asks with ?v= the file's ETag: a new version is a new URL,
	// so a preview can be kept as long as it lives.
	if r.URL.Query().Get("v") == n.ETag {
		hdr.Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		hdr.Set("Cache-Control", "private, no-cache")
	}
	hdr.Set("ETag", quoteETag(n.ETag+"-"+string(size)))
	if etagMatches(r.Header.Get("If-None-Match"), quoteETag(n.ETag+"-"+string(size))) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if _, err := io.Copy(w, rc); err != nil {
		slog.DebugContext(r.Context(), "nodes: preview interrupted", "node", n.ID, "err", err)
	}
}
