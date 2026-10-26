package nodes

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/httpx"
)

const (
	// maxNodeBody fits a name (255 bytes, escaped) and an id as JSON.
	maxNodeBody = 4 << 10
	// MaxBatchOps bounds one batch: its operations run one after another
	// within the request.
	MaxBatchOps  = 1000
	maxBatchBody = 256 << 10
)

type pathEntryJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type nodeWithPath struct {
	nodeJSON
	// Path runs from the root (name "") down to the node itself.
	Path []pathEntryJSON `json:"path"`
}

// get returns a node and the folders leading to it, for breadcrumbs.
func (h *Handlers) get(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	id, err := h.resolveID(r, u)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	n, err := h.FS.Get(r.Context(), u.ID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	path, err := h.FS.Path(r.Context(), u.ID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, withPath(n, path))
}

// patch renames and/or moves a node: {"name"?, "parentId"?}. With
// If-Match it does so only if the node still has that ETag (a sync
// client's base), else 412.
func (h *Handlers) patch(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	var req struct {
		Name     string `json:"name"`
		ParentID string `json:"parentId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" && req.ParentID == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, `expected {"name"} and/or {"parentId"}`)
		return
	}
	id, err := h.resolveID(r, u)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	n, err := h.place(r, u, id, req.ParentID, req.Name, ifMatch(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toJSON(n))
}

// place moves id into parentID (or keeps its folder if "") under name (or
// keeps its name if ""), if it has one of the ETags in cond (if any).
func (h *Handlers) place(r *http.Request, u auth.User, id, parentID, name string, cond []string) (fs.Node, error) {
	if parentID != "" {
		var err error
		if parentID, err = h.resolve(r.Context(), u, parentID); err != nil {
			return fs.Node{}, err
		}
	}
	return h.FS.MoveIfMatch(r.Context(), u.ID, id, parentID, name, cond)
}

// ifMatch reads the If-Match header into unquoted ETags; weak ones count
// as their strong form (fs ETags are all strong).
func ifMatch(r *http.Request) []string {
	var tags []string
	for tag := range strings.SplitSeq(r.Header.Get("If-Match"), ",") {
		tag = strings.TrimPrefix(strings.TrimSpace(tag), "W/")
		if tag != "" {
			tags = append(tags, strings.Trim(tag, `"`))
		}
	}
	return tags
}

// mkdir creates a folder in {id}: {"name"}.
func (h *Handlers) mkdir(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, `expected {"name"}`)
		return
	}
	parentID, err := h.resolveID(r, u)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	n, err := h.FS.CreateDir(r.Context(), u.ID, parentID, req.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toJSON(n))
}

type batchOp struct {
	Op       string `json:"op"`
	ID       string `json:"id"`
	ParentID string `json:"parentId"`
	Name     string `json:"name"`
}

type batchResult struct {
	ID   string    `json:"id"`
	Node *nodeJSON `json:"node,omitempty"`
	// Job is a copy going on in the background (GET /jobs/{id}).
	Job   *int64           `json:"job,omitempty"`
	Error *httpx.ErrorBody `json:"error,omitempty"`
}

// batch runs several operations, each on its own: one failing (a name
// conflict, say) doesn't undo or stop the others, and every result says
// how its operation went, in request order. All-or-nothing would mean
// holding the locks of every chain involved and undoing renames on disk.
//
// Operations: "move" ({"id","parentId","name"?}) and "delete"
// ({"id"}, to the trash). A delete's result carries no node. Copy joins
// them with E1-6.
func (h *Handlers) batch(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	var req struct {
		Ops []batchOp `json:"ops"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Ops) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, `expected {"ops": [...]}`)
		return
	}
	if len(req.Ops) > MaxBatchOps {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "at most 1000 operations per batch")
		return
	}
	// Checked upfront so a typo fails the batch before anything changes.
	for _, op := range req.Ops {
		valid := op.ID != "" && ((op.Op == "move" || op.Op == "copy") && op.ParentID != "" || op.Op == "delete")
		if !valid {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, `each operation must be {"op":"move"|"copy","id","parentId"} or {"op":"delete","id"}`)
			return
		}
	}
	resp := struct {
		Results []batchResult `json:"results"`
	}{make([]batchResult, len(req.Ops))}
	for i, op := range req.Ops {
		resp.Results[i].ID = op.ID
		var n fs.Node
		var err error
		if op.Op == "delete" {
			var name string
			if n, gerr := h.FS.Get(r.Context(), u.ID, op.ID); gerr == nil {
				name = n.Name
			}
			if err = h.FS.Trash(r.Context(), u.ID, op.ID); err == nil {
				h.record(r, audit.NodeTrash, op.ID, name, nil)
			}
		} else if op.Op == "copy" {
			var job int64
			if n, job, err = h.copyNode(r, u, op); err == nil && job != 0 {
				resp.Results[i].Job = &job
				continue
			}
		} else {
			n, err = h.place(r, u, op.ID, op.ParentID, op.Name, nil)
		}
		if err != nil {
			e, status := apiError(err)
			if status == http.StatusInternalServerError {
				slog.ErrorContext(r.Context(), "nodes: batch operation failed", "node", op.ID, "err", err)
			}
			resp.Results[i].Error = &e
			continue
		}
		if op.Op != "delete" {
			j := toJSON(n)
			resp.Results[i].Node = &j
		}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}
