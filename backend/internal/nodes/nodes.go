// Package nodes serves the REST API of the file tree: /api/v1/nodes/*,
// /api/v1/versions/*, /api/v1/trash, /api/v1/search, and the shares on
// nodes: /api/v1/nodes/{id}/shares, /api/v1/shares/*, /api/v1/shared-with-me
// (architecture §11). It only translates HTTP to the fs service and back:
// access checks and every rule about the tree live in fs.
package nodes

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/httpx"
	"github.com/do-service/cloud/backend/internal/preview"
	"github.com/do-service/cloud/backend/internal/ratelimit"
	"github.com/do-service/cloud/backend/internal/share"
	"github.com/do-service/cloud/backend/internal/upload"
)

type Handlers struct {
	FS     *fs.Service
	Shares *share.Service
	// Links, when set, serves public links: their owner's API and the
	// public side under /s/{token}/api.
	Links *share.Links
	// Uploads takes uploads through upload links.
	Uploads *upload.Handlers
	// UnlockLimiter bounds password attempts on a link per address.
	UnlockLimiter *ratelimit.Limiter
	// SecureCookies marks link cookies Secure (config.CookieSecure).
	SecureCookies bool
	// SignedIn wraps a route so it runs only for a signed-in user, found
	// in the request context (auth.Middleware + auth.RequireUser).
	SignedIn func(http.Handler) http.Handler
	// Audit records deletions, shares and links; nil records nothing.
	Audit *audit.Log
	// Preview makes and serves previews; nil leaves the routes out.
	Preview *preview.Service
	// Jobs runs big copies in the background; nil copies everything
	// within the request.
	Jobs CopyJobs
}

// record writes an audit entry by the request's user about node id, named
// as it is now (before a deletion) or name if given.
func (h *Handlers) record(r *http.Request, action, id, name string, details map[string]any) {
	if h.Audit == nil {
		return
	}
	if name == "" {
		u, _ := auth.UserFromContext(r.Context())
		if n, err := h.FS.Get(r.Context(), u.ID, id); err == nil {
			name = n.Name
		}
	}
	h.Audit.Record(r, audit.Event{Actor: auth.Actor(r.Context()), Action: action, TargetID: id, TargetName: name, Details: details})
}

// rootAlias stands for the caller's root folder wherever a folder id goes,
// so a client can start browsing without knowing that id.
const rootAlias = "root"

func (h *Handlers) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/nodes/{id}/children", h.SignedIn(http.HandlerFunc(h.children)))
	// GET patterns match HEAD too, which content answers without a body.
	mux.Handle("GET /api/v1/nodes/{id}/content", h.SignedIn(http.HandlerFunc(h.content)))
	mux.Handle("GET /api/v1/nodes/{id}/archive", h.SignedIn(http.HandlerFunc(h.archive)))
	if h.Preview != nil {
		mux.Handle("GET /api/v1/nodes/{id}/preview", h.SignedIn(http.HandlerFunc(h.preview)))
	}
	mux.Handle("GET /api/v1/nodes/{id}", h.SignedIn(http.HandlerFunc(h.get)))
	mux.Handle("PATCH /api/v1/nodes/{id}", httpx.LimitBody(maxNodeBody)(h.SignedIn(http.HandlerFunc(h.patch))))
	mux.Handle("POST /api/v1/nodes/{id}/children", httpx.LimitBody(maxNodeBody)(h.SignedIn(http.HandlerFunc(h.mkdir))))
	mux.Handle("POST /api/v1/nodes/batch", httpx.LimitBody(maxBatchBody)(h.SignedIn(http.HandlerFunc(h.batch))))
	mux.Handle("DELETE /api/v1/nodes/{id}", h.SignedIn(http.HandlerFunc(h.trash)))
	mux.Handle("GET /api/v1/trash", h.SignedIn(http.HandlerFunc(h.listTrash)))
	mux.Handle("POST /api/v1/trash/{id}/restore", h.SignedIn(http.HandlerFunc(h.restore)))
	mux.Handle("DELETE /api/v1/trash/{id}", h.SignedIn(http.HandlerFunc(h.purge)))
	mux.Handle("DELETE /api/v1/trash", h.SignedIn(http.HandlerFunc(h.emptyTrash)))
	mux.Handle("GET /api/v1/search", h.SignedIn(http.HandlerFunc(h.search)))
	if h.Jobs != nil {
		mux.Handle("GET /api/v1/jobs/{id}", h.SignedIn(http.HandlerFunc(h.jobStatus)))
	}
	mux.Handle("GET /api/v1/me/quota", h.SignedIn(http.HandlerFunc(h.quota)))
	mux.Handle("GET /api/v1/nodes/{id}/versions", h.SignedIn(http.HandlerFunc(h.versions)))
	mux.Handle("GET /api/v1/versions/{id}/content", h.SignedIn(http.HandlerFunc(h.versionContent)))
	mux.Handle("POST /api/v1/versions/{id}/restore", h.SignedIn(http.HandlerFunc(h.restoreVersion)))
	if h.Links != nil {
		h.registerLinks(mux)
	}
	if h.Shares != nil {
		mux.Handle("GET /api/v1/nodes/{id}/shares", h.SignedIn(http.HandlerFunc(h.listShares)))
		mux.Handle("POST /api/v1/nodes/{id}/shares", httpx.LimitBody(maxNodeBody)(h.SignedIn(http.HandlerFunc(h.grant))))
		mux.Handle("PATCH /api/v1/shares/{id}", httpx.LimitBody(maxNodeBody)(h.SignedIn(http.HandlerFunc(h.updateShare))))
		mux.Handle("DELETE /api/v1/shares/{id}", h.SignedIn(http.HandlerFunc(h.revoke)))
		mux.Handle("GET /api/v1/shared-with-me", h.SignedIn(http.HandlerFunc(h.sharedWithMe)))
	}
}

// resolveID is the {id} path value, with rootAlias replaced by the id of
// u's root.
func (h *Handlers) resolveID(r *http.Request, u auth.User) (string, error) {
	return h.resolve(r.Context(), u, r.PathValue("id"))
}

// resolve replaces rootAlias with the id of u's root.
func (h *Handlers) resolve(ctx context.Context, u auth.User, id string) (string, error) {
	if id != rootAlias {
		return id, nil
	}
	root, err := h.FS.Root(ctx, u.ID)
	return root.ID, err
}

type nodeJSON struct {
	ID string `json:"id"`
	// ParentID is null for a root, and for the top of what the caller
	// sees (a node shared with them, a link's folder) where the response
	// knows it is one: the folder above is none of their business.
	ParentID *string   `json:"parentId"`
	Name     string    `json:"name"`
	Kind     fs.Kind   `json:"kind"`
	Size     int64     `json:"size"`
	MIME     string    `json:"mime,omitempty"`
	ETag     string    `json:"etag"`
	SHA256   string    `json:"sha256,omitempty"`
	MTime    time.Time `json:"mtime"`
	// Permissions are the caller's rights on it: what the UI may offer.
	Permissions []string `json:"permissions"`
	// Shared marks a node shared on its own (with someone or by a link),
	// in listings, for those who may manage its shares.
	Shared bool `json:"shared,omitempty"`
}

func toJSON(n fs.Node) nodeJSON {
	j := nodeJSON{ID: n.ID, Name: n.Name, Kind: n.Kind, Size: n.Size, MIME: n.MIME, ETag: n.ETag, MTime: n.MTime, Permissions: permNames(n.Perms)}
	if n.ParentID != "" {
		j.ParentID = &n.ParentID
	}
	// A sync client tells an edit from a touch by it, and skips
	// downloading what it already has.
	if len(n.ContentHash) > 0 {
		j.SHA256 = hex.EncodeToString(n.ContentHash)
	}
	return j
}

// withPath is n as JSON with its path (fs.Path), its parent hidden when
// the path starts at n itself.
func withPath(n fs.Node, path []fs.PathEntry) nodeWithPath {
	resp := nodeWithPath{nodeJSON: toJSON(n), Path: make([]pathEntryJSON, len(path))}
	for i, e := range path {
		resp.Path[i] = pathEntryJSON(e)
	}
	if len(path) < 2 {
		resp.ParentID = nil
	}
	return resp
}

// permNames spells a rights mask out for JSON.
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

// parsePerms is permNames' inverse; ok is false for an unknown name.
func parsePerms(names []string) (p access.Perm, ok bool) {
	for _, name := range names {
		switch name {
		case "read":
			p |= access.Read
		case "write":
			p |= access.Write
		case "reshare":
			p |= access.Reshare
		default:
			return 0, false
		}
	}
	return p, true
}

type childrenResponse struct {
	Items []nodeJSON `json:"items"`
	// NextCursor is null on the last page.
	NextCursor *string `json:"nextCursor"`
}

// children lists a folder: ?sort=name|mtime|size&order=asc|desc&limit=&cursor=.
func (h *Handlers) children(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	id, err := h.resolveID(r, u)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.listChildren(w, r, u.ID, id)
}

// listChildren answers a listing of folder id as actorID sees it, with the
// request's sort, order, limit and cursor.
func (h *Handlers) listChildren(w http.ResponseWriter, r *http.Request, actorID, id string) {
	q := r.URL.Query()
	opts := fs.ListOptions{Sort: fs.Sort(q.Get("sort")), Cursor: q.Get("cursor")}
	if opts.Sort == "" {
		opts.Sort = fs.SortName
	}
	if !opts.Sort.Valid() {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "sort must be name, mtime or size")
		return
	}
	switch q.Get("order") {
	case "", "asc":
	case "desc":
		opts.Desc = true
	default:
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "order must be asc or desc")
		return
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "limit must be a positive number")
			return
		}
		opts.Limit = n
	}

	items, next, err := h.FS.List(r.Context(), actorID, id, opts)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	shared, err := h.sharedAmong(r.Context(), items)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := childrenResponse{Items: make([]nodeJSON, 0, len(items))}
	for _, n := range items {
		j := toJSON(n)
		j.Shared = shared[n.ID]
		resp.Items = append(resp.Items, j)
	}
	if next != "" {
		resp.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// sharedAmong tells which of a listing's items are shared, among those
// whose shares the caller may see (reshare, as GET .../shares asks): a mere
// recipient isn't told what else its owner shares, nor a link's visitor.
// Checked per item: a nested share can give one child more rights than its
// siblings have.
func (h *Handlers) sharedAmong(ctx context.Context, items []fs.Node) (map[string]bool, error) {
	if h.Shares == nil {
		return nil, nil
	}
	var ids []string
	for _, n := range items {
		if n.Perms.Has(access.Reshare) {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return h.Shares.Shared(ctx, ids)
}

// fail answers with the API error for an fs error.
func (h *Handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	e, status := apiError(err)
	if status == http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "nodes: request failed", "err", err)
	}
	httpx.WriteError(w, status, e.Code, e.Message)
}

// apiError maps an fs error to what the API reports, for whole responses
// and for single batch operations alike. Unknown errors are internal and
// say nothing more.
func apiError(err error) (httpx.ErrorBody, int) {
	for _, m := range []struct {
		err    error
		status int
		code   string
		msg    string
	}{
		{fs.ErrNotFound, http.StatusNotFound, httpx.CodeNotFound, "no such file or folder"},
		{fs.ErrNotDir, http.StatusBadRequest, httpx.CodeNotADirectory, "not a folder"},
		{fs.ErrNotFile, http.StatusBadRequest, httpx.CodeNotAFile, "not a file"},
		{fs.ErrInvalidCursor, http.StatusBadRequest, httpx.CodeInvalidRequest, "cursor is malformed or from another sort order"},
		{fs.ErrNameConflict, http.StatusConflict, httpx.CodeNameConflict, "a file or folder with this name already exists here"},
		{fs.ErrInvalidName, http.StatusBadRequest, httpx.CodeInvalidName, "name is empty, too long, or contains / or control characters"},
		{fs.ErrCycle, http.StatusConflict, httpx.CodeMoveIntoItself, "a folder can't be moved into itself or its subfolder"},
		{fs.ErrIsRoot, http.StatusBadRequest, httpx.CodeRootImmutable, "the root folder can't be moved or renamed"},
		{fs.ErrQueryTooShort, http.StatusBadRequest, httpx.CodeQueryTooShort, "search for at least 3 characters"},
		{fs.ErrForbidden, http.StatusForbidden, httpx.CodeForbidden, "your rights on this don't allow it"},
		{fs.ErrLocked, http.StatusLocked, httpx.CodeLocked, "it is locked: open in an editor or by a sync client"},
		{fs.ErrQuotaExceeded, http.StatusRequestEntityTooLarge, httpx.CodeQuotaExceeded, "not enough quota left for this"},
		{fs.ErrCopyTooLarge, http.StatusRequestEntityTooLarge, httpx.CodeInvalidRequest, "the folder is too big to copy at once"},
		{fs.ErrPrecondition, http.StatusPreconditionFailed, httpx.CodePreconditionFailed, "it has changed since the ETag in If-Match"},
		{fs.ErrCrossOwner, http.StatusConflict, httpx.CodeCrossOwnerMove, "files can't be moved between different owners; copy them instead"},
		{share.ErrNoShare, http.StatusNotFound, httpx.CodeNotFound, "no such share"},
		{share.ErrInvalidGrantee, http.StatusBadRequest, httpx.CodeInvalidGrantee, "no such user or group to share with"},
		{share.ErrNotRecipients, http.StatusBadRequest, httpx.CodeInvalidGrantee, "only users and groups can be shared with"},
		{share.ErrInvalidPerms, http.StatusBadRequest, httpx.CodeInvalidPermissions, "rights must include read and not exceed your own"},
		{share.ErrNoLink, http.StatusNotFound, httpx.CodeNotFound, "no such link"},
		{share.ErrInvalidMode, http.StatusBadRequest, httpx.CodeInvalidRequest, "mode must be view, download or upload; upload needs a folder you can write to"},
		{share.ErrPasswordLen, http.StatusBadRequest, httpx.CodeInvalidRequest, "link password must be 1 to 256 characters"},
		{share.ErrLinksDisabled, http.StatusConflict, httpx.CodeLinksDisabled, "public links need SECRET_KEY to be set on the server"},
	} {
		if errors.Is(err, m.err) {
			return httpx.ErrorBody{Code: m.code, Message: m.msg}, m.status
		}
	}
	return httpx.ErrorBody{Code: httpx.CodeInternal, Message: "internal server error"}, http.StatusInternalServerError
}
