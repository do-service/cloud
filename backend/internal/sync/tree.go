package sync

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/httpx"
)

// rootAlias stands for the caller's root folder, as in the nodes API.
const rootAlias = "root"

type cursorLine struct {
	Cursor string `json:"cursor"`
}

// tree streams the subtree of ?root= (any folder the caller sees, or a
// shared file) as NDJSON: {"cursor"} first, then a node a line, the root
// first and every folder before its contents, all of one snapshot. The
// cursor is of the same moment: changes from it carry on from the tree.
// A failure midway cuts the response off, so a truncated tree can't pass
// for a whole one.
func (h *Handlers) tree(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := auth.UserFromContext(ctx)
	id := r.URL.Query().Get("root")
	switch id {
	case "":
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "root is required")
		return
	case rootAlias:
		root, err := h.FS.Root(ctx, u.ID)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		id = root.ID
	}

	bw := bufio.NewWriterSize(w, 64<<10)
	enc := json.NewEncoder(bw)
	started := false
	err := h.FS.SyncTree(ctx, u.ID, id, func(view fs.SyncView, root fs.Node) error {
		top, err := h.isTop(ctx, u.ID, view, root)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		started = true
		if err := enc.Encode(cursorLine{Cursor: cursorAt(view).encode()}); err != nil {
			return err
		}
		return enc.Encode(toJSON(root, top))
	}, func(n fs.Node) error {
		return enc.Encode(toJSON(n, false))
	})
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		return
	}
	if !started {
		h.fail(w, r, err)
		return
	}
	if ctx.Err() == nil {
		slog.ErrorContext(ctx, "sync: tree cut off", "err", err)
	}
	panic(http.ErrAbortHandler)
}
