// Package upload serves resumable uploads over tus (R2) at
// /api/v1/uploads/, with tusd as the protocol engine and fs deciding
// everything else: where a file may go, the quota it reserves, and what
// it becomes once complete (architecture §5).
//
// tusd writes the bytes to data/uploads/tus, on the same filesystem as the
// tree, so a finished upload becomes a file with one rename.
package upload

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	tusd "github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"
	xslog "golang.org/x/exp/slog"

	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/httpx"
)

const (
	// BasePath is where the tus endpoint lives; uploads are BasePath+id.
	BasePath = "/api/v1/uploads/"
	// stagingArea is tusd's directory under the data dir, and stagingKey
	// the matching storage key prefix fs reads and moves uploads by.
	stagingArea = "uploads/tus"
	// NodeIDHeader on the response that completes an upload names the
	// file it became, and NodeETagHeader gives that file's new ETag
	// (unquoted, as in the nodes API): a sync client's next base, without
	// another request.
	NodeIDHeader   = "Upload-Node-Id"
	NodeETagHeader = "Upload-Node-ETag"
)

// Metadata keys a client sets in Upload-Metadata.
const (
	metaFilename   = "filename"
	metaParentID   = "parentId"
	metaSHA256     = "sha256"
	metaOnConflict = "onConflict"
	// metaNodeID makes the upload a new version of that file, found by id
	// (fs.Upload.NodeID); filename, parentId and onConflict are ignored.
	metaNodeID = "nodeId"
	// metaIfMatch, with nodeId only, is the file's ETag the client based
	// its change on; a mismatch at creation or finish is 412.
	metaIfMatch = "ifMatch"
	// metaMTime is the file's modification time, RFC 3339.
	metaMTime = "mtime"
)

// mtimeSkew is how far in the future an mtime may be: clocks of client and
// server disagree a little. Beyond it the client's clock is broken, and
// the time would sort the file first for years.
const mtimeSkew = 24 * time.Hour

// rootAlias stands for the uploader's root folder as parentId.
const rootAlias = "root"

type Handlers struct {
	fs       *fs.Service
	signedIn func(http.Handler) http.Handler
	store    *hashStore
	tus      *tusd.Handler
}

// New prepares the tus endpoint: files under dataDir (config.DataDir),
// signedIn wrapping it so only a signed-in user gets through.
func New(files *fs.Service, dataDir string, signedIn func(http.Handler) http.Handler) (*Handlers, error) {
	dir := filepath.Join(dataDir, filepath.FromSlash(stagingArea))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create upload dir: %w", err)
	}
	h := &Handlers{fs: files, signedIn: signedIn, store: newHashStore(dir)}
	composer := tusd.NewStoreComposer()
	h.store.UseIn(composer)
	// One process serves an upload at a time; a second PATCH for the same
	// upload waits for, or takes over from, the first.
	memorylocker.New().UseIn(composer)

	handler, err := tusd.NewHandler(tusd.Config{
		BasePath:      BasePath,
		StoreComposer: composer,
		// Content goes out through /nodes/{id}/content once it is a file.
		DisableDownload: true,
		// Parallel parts arrive with E3-4.
		DisableConcatenation:      true,
		Cors:                      &tusd.CorsConfig{Disable: true},
		PreUploadCreateCallback:   h.preCreate,
		PreFinishResponseCallback: h.preFinish,
		PreUploadTerminateCallback: func(ev tusd.HookEvent) (tusd.HTTPResponse, error) {
			return tusd.HTTPResponse{}, h.fs.CancelUpload(ev.Context, actorFrom(ev.Context).principal, ev.Upload.ID)
		},
		// tusd logs through x/exp/slog, not log/slog, so it can't share
		// the app's logger; per-request lines would duplicate the app's
		// own request log anyway, so it only reports problems.
		Logger: xslog.New(xslog.NewJSONHandler(os.Stdout, &xslog.HandlerOptions{Level: xslog.LevelWarn})).With("component", "tus"),
	})
	if err != nil {
		return nil, fmt.Errorf("tus handler: %w", err)
	}
	h.tus = handler
	return h, nil
}

// Register mounts the endpoint on mux, with and without the trailing
// slash: clients POST to either form.
func (h *Handlers) Register(mux *http.ServeMux) {
	mount := strings.TrimSuffix(BasePath, "/")
	tus := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.tus.ServeHTTP(&locationRewriter{ResponseWriter: w, from: BasePath, to: BasePath}, r)
	})
	endpoint := h.signedIn(h.ownUploadsOnlyAt(mount, http.StripPrefix(mount, tus)))
	mux.Handle(strings.TrimSuffix(BasePath, "/"), endpoint)
	mux.Handle(BasePath, endpoint)
}

// ownUploadsOnlyAt answers 404 for an upload id under mount that isn't
// the actor's, before tusd sees the request: tusd knows nothing about
// users or links.
func (h *Handlers) ownUploadsOnlyAt(mount string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, mount), "/")
		if id != "" {
			if _, err := h.fs.GetUpload(r.Context(), actorFrom(r.Context()).principal, id); err != nil {
				if !errors.Is(err, fs.ErrNotFound) {
					slog.ErrorContext(r.Context(), "upload: look up", "err", err)
					httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
					return
				}
				httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such upload")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// preCreate turns the creation request into a reservation (fs.ReserveUpload)
// and gives the upload the reservation's id.
func (h *Handlers) preCreate(ev tusd.HookEvent) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
	who := actorFrom(ev.Context)
	if ev.Upload.SizeIsDeferred {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, apiError(http.StatusBadRequest, httpx.CodeInvalidRequest, "Upload-Length is required: the quota is reserved upfront")
	}
	meta := ev.Upload.MetaData
	up := fs.Upload{
		ParentID:   meta[metaParentID],
		Name:       meta[metaFilename],
		Size:       ev.Upload.Size,
		OnConflict: fs.Conflict(meta[metaOnConflict]),
		NodeID:     meta[metaNodeID],
		IfMatch:    strings.Trim(meta[metaIfMatch], `"`),
	}
	switch {
	case up.NodeID != "" && who.root != "":
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, apiError(http.StatusBadRequest, httpx.CodeInvalidRequest, "uploads through a link can't replace files")
	case up.IfMatch != "" && up.NodeID == "":
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, apiError(http.StatusBadRequest, httpx.CodeInvalidRequest, "ifMatch needs nodeId")
	case up.NodeID != "":
		// The file decides where the content goes and under what name.
		up.ParentID, up.Name, up.OnConflict = "", "", fs.ConflictReplace
	case up.OnConflict == "" || who.root != "":
		// Through a link it is always "keep both": an anonymous uploader
		// replaces nothing and is told nothing about what's there.
		up.OnConflict = fs.ConflictRename
	}
	if !up.OnConflict.Valid() {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, apiError(http.StatusBadRequest, httpx.CodeInvalidRequest, "onConflict must be fail, rename or replace")
	}
	if s := meta[metaSHA256]; s != "" {
		sum, err := hex.DecodeString(s)
		if err != nil || len(sum) != 32 {
			return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, apiError(http.StatusBadRequest, httpx.CodeInvalidRequest, "sha256 must be 64 hex digits")
		}
		up.SHA256 = sum
	}
	if s := meta[metaMTime]; s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil || t.Before(time.Unix(0, 0)) || t.After(time.Now().Add(mtimeSkew)) {
			return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, apiError(http.StatusBadRequest, httpx.CodeInvalidRequest, "mtime must be an RFC 3339 time from 1970 up to a day ahead")
		}
		up.MTime = t
	}
	toRoot := up.NodeID == "" && (up.ParentID == rootAlias || up.ParentID == "")
	if toRoot && who.root != "" {
		up.ParentID = who.root
	} else if toRoot {
		root, err := h.fs.Root(ev.Context, who.principal)
		if err != nil {
			return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, fsError(ev.Context, err)
		}
		up.ParentID = root.ID
	}
	up, err := h.fs.ReserveUpload(ev.Context, who.principal, up)
	if err != nil {
		return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, fsError(ev.Context, err)
	}
	return tusd.HTTPResponse{}, tusd.FileInfoChanges{ID: up.ID}, nil
}

// preFinish makes the complete upload a file before the last response
// goes out, so the client learns the result (and the node's id) from it.
// A failure discards the upload: nothing the client can resend would fix a
// wrong checksum, a vanished folder or a name conflict it asked to fail on.
func (h *Handlers) preFinish(ev tusd.HookEvent) (tusd.HTTPResponse, error) {
	who := actorFrom(ev.Context)
	// The file is moved, not copied: the request may already be gone.
	ctx := context.WithoutCancel(ev.Context)
	// Taken before the data moves: filestore can't open an upload whose
	// data is gone, and its .info file and our sidecar have to go too.
	stored, err := h.store.GetUpload(ctx, ev.Upload.ID)
	if err != nil {
		return tusd.HTTPResponse{}, fsError(ctx, err)
	}
	key := stagingArea + "/" + ev.Upload.ID
	var node fs.Node
	if sum, ok := h.store.sum(ctx, ev.Upload.ID, ev.Upload.Size); ok {
		node, err = h.fs.FinishUploadHashed(ctx, who.principal, ev.Upload.ID, key, sum)
	} else {
		node, err = h.fs.FinishUpload(ctx, who.principal, ev.Upload.ID, key)
	}
	if terr := h.store.AsTerminatableUpload(stored).Terminate(ctx); terr != nil {
		slog.ErrorContext(ctx, "upload: remove leftovers", "upload", ev.Upload.ID, "err", terr)
	}
	if err != nil {
		if cerr := h.fs.CancelUpload(ctx, who.principal, ev.Upload.ID); cerr != nil {
			slog.ErrorContext(ctx, "upload: drop reservation", "upload", ev.Upload.ID, "err", cerr)
		}
		return tusd.HTTPResponse{}, fsError(ctx, err)
	}
	return tusd.HTTPResponse{Header: tusd.HTTPHeader{NodeIDHeader: node.ID, NodeETagHeader: node.ETag}}, nil
}

// apiError is a tusd error whose response is the API's {"code","message"}.
func apiError(status int, code, message string) tusd.Error {
	body, _ := json.Marshal(httpx.ErrorBody{Code: code, Message: message})
	return tusd.Error{
		ErrorCode: code,
		Message:   message,
		HTTPResponse: tusd.HTTPResponse{
			StatusCode: status,
			Body:       string(body),
			Header:     tusd.HTTPHeader{"Content-Type": "application/json; charset=utf-8"},
		},
	}
}

// fsError maps an fs error to the API's error for it.
func fsError(ctx context.Context, err error) error {
	for _, m := range []struct {
		err    error
		status int
		code   string
		msg    string
	}{
		{fs.ErrNotFound, http.StatusNotFound, httpx.CodeNotFound, "no such folder or file"},
		{fs.ErrNotDir, http.StatusBadRequest, httpx.CodeNotADirectory, "not a folder"},
		{fs.ErrNotFile, http.StatusBadRequest, httpx.CodeNotAFile, "nodeId is a folder, not a file"},
		{fs.ErrPrecondition, http.StatusPreconditionFailed, httpx.CodePreconditionFailed, "the file has changed since the ETag in ifMatch"},
		{fs.ErrInvalidName, http.StatusBadRequest, httpx.CodeInvalidName, "name is empty, too long, or contains / or control characters"},
		{fs.ErrNameConflict, http.StatusConflict, httpx.CodeNameConflict, "a file or folder with this name already exists here"},
		{fs.ErrQuotaExceeded, http.StatusRequestEntityTooLarge, httpx.CodeQuotaExceeded, "not enough quota left for this upload"},
		{fs.ErrChecksumMismatch, http.StatusConflict, httpx.CodeChecksumMismatch, "the uploaded content doesn't match its sha256"},
		{fs.ErrSizeMismatch, http.StatusConflict, httpx.CodeChecksumMismatch, "the uploaded content doesn't match its size"},
		{fs.ErrLocked, http.StatusLocked, httpx.CodeLocked, "the file is locked: open in an editor or by a sync client"},
		// A folder shared read-only: seen, not written to.
		{fs.ErrForbidden, http.StatusForbidden, httpx.CodeForbidden, "your rights on this folder don't allow uploading"},
	} {
		if errors.Is(err, m.err) {
			return apiError(m.status, m.code, m.msg)
		}
	}
	slog.ErrorContext(ctx, "upload failed", "err", err)
	return apiError(http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
}

// actor is who uploads: a signed-in user, or a public link with the folder
// it opens to.
type actor struct {
	principal string
	// root is the link's folder, "" for a user.
	root string
}

type linkKey struct{}

// actorFrom is the link ServeLink put in ctx, else the signed-in user.
func actorFrom(ctx context.Context) actor {
	if a, ok := ctx.Value(linkKey{}).(actor); ok {
		return a
	}
	u, _ := auth.UserFromContext(ctx)
	return actor{principal: u.ID}
}

// ServeLink serves the tus endpoint for uploads through a public link,
// mounted at mount ("/s/{token}/api/uploads"): principal is the link's
// (access.LinkPrincipal), root the folder it opens to. The caller has
// checked the link is live, unlocked and in upload mode.
func (h *Handlers) ServeLink(w http.ResponseWriter, r *http.Request, mount, principal, root string) {
	ctx := context.WithValue(r.Context(), linkKey{}, actor{principal: principal, root: root})
	// tusd builds Location from its BasePath; the client must come back
	// through the link's mount.
	rw := &locationRewriter{ResponseWriter: w, from: BasePath, to: mount + "/"}
	h.ownUploadsOnlyAt(mount, http.StripPrefix(mount, h.tus)).ServeHTTP(rw, r.WithContext(ctx))
}

// locationRewriter moves tusd's Location header under another mount, and
// makes it a path: tusd prefixes the scheme and host it was asked on,
// which behind a TLS-terminating proxy is http://, an address a page on
// https:// may not call. The client resolves the path against its own.
type locationRewriter struct {
	http.ResponseWriter
	from, to string
}

func (w *locationRewriter) WriteHeader(status int) {
	if loc := w.Header().Get("Location"); loc != "" {
		if i := strings.Index(loc, w.from); i >= 0 {
			w.Header().Set("Location", w.to+loc[i+len(w.from):])
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the connection, which tusd
// uses to extend deadlines and flush.
func (w *locationRewriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
