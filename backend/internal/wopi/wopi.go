// Package wopi is the WOPI host (R12): online editors (Collabora,
// OnlyOffice) read and save files through it, on behalf of the user an
// access token was issued to. Every operation goes through fs, as that
// user, so the editor gets exactly the user's rights; saving makes a new
// version (R7), and WOPI locks are fs's locks, shared with WebDAV.
package wopi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/httpx"
)

// Handlers serves the WOPI endpoints under /wopi and the web UI's way into
// the editor under /api/v1.
type Handlers struct {
	FS        *fs.Service
	Tokens    *Tokens
	Discovery *Discovery
	// HostURL is this server's origin as the editor's server reaches it:
	// what WOPISrc and the proof's URL start with.
	HostURL string
	// PublicOrigin is the web UI's origin, which the editor's frame posts
	// messages to.
	PublicOrigin string
	SignedIn     func(http.Handler) http.Handler
	// now is time.Now, replaceable in tests.
	now func() time.Time
}

func (h *Handlers) Register(mux *http.ServeMux) {
	wopi := func(f func(http.ResponseWriter, *http.Request, grant)) http.Handler { return h.withToken(f) }
	mux.Handle("GET /wopi/files/{id}", wopi(h.checkFileInfo))
	mux.Handle("GET /wopi/files/{id}/contents", wopi(h.getFile))
	mux.Handle("POST /wopi/files/{id}/contents", wopi(h.putFile))
	mux.Handle("POST /wopi/files/{id}", wopi(h.override))
	mux.Handle("GET /api/v1/editor", h.SignedIn(http.HandlerFunc(h.editorInfo)))
	mux.Handle("POST /api/v1/nodes/{id}/edit", h.SignedIn(http.HandlerFunc(h.edit)))
	mux.Handle("POST /api/v1/nodes/{id}/documents", httpx.LimitBody(4<<10)(h.SignedIn(http.HandlerFunc(h.createDocument))))
}

func (h *Handlers) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// withToken authenticates a WOPI request: a live access token for the
// file in the path, and the editor's signature over the request.
func (h *Handlers) withToken(f func(http.ResponseWriter, *http.Request, grant)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("access_token")
		if token == "" {
			token, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		g, err := h.Tokens.resolve(r.Context(), token)
		if err != nil {
			if !errors.Is(err, errNoToken) {
				slog.ErrorContext(r.Context(), "wopi: resolve token", "err", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// A token opens its own file only.
		if g.NodeID != r.PathValue("id") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		editor, err := h.Discovery.Editor(r.Context())
		if err != nil {
			slog.ErrorContext(r.Context(), "wopi: discovery", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		verify := func(e *Editor) error {
			return verifyProof(e, token, h.HostURL+r.URL.RequestURI(), r.Header.Get("X-WOPI-TimeStamp"),
				r.Header.Get("X-WOPI-Proof"), r.Header.Get("X-WOPI-ProofOld"), h.clock())
		}
		err = verify(editor)
		if err != nil {
			// The editor may have rotated its key since the discovery was
			// cached: check again with a fresh one.
			if fresh := h.Discovery.Refetch(r.Context()); fresh != editor {
				err = verify(fresh)
			}
		}
		// The spec's answer to a bad signature is 500.
		if err != nil {
			slog.WarnContext(r.Context(), "wopi: request with a bad proof", "node", g.NodeID)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f(w, r, g)
	})
}

// status maps an fs error to a WOPI status.
func status(ctx context.Context, err error) int {
	switch {
	case errors.Is(err, fs.ErrNotFound):
		return http.StatusNotFound
	// The token is the user's: what they may not do, it doesn't grant.
	case errors.Is(err, fs.ErrForbidden):
		return http.StatusUnauthorized
	case errors.Is(err, fs.ErrLocked):
		return http.StatusConflict
	case errors.Is(err, fs.ErrQuotaExceeded):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, fs.ErrNotFile), errors.Is(err, fs.ErrBadLockID), errors.Is(err, fs.ErrSizeMismatch):
		return http.StatusBadRequest
	}
	slog.ErrorContext(ctx, "wopi: request failed", "err", err)
	return http.StatusInternalServerError
}

// modified is a file's LastModifiedTime as WOPI shows it; Collabora sends
// it back on saving to detect changes made meanwhile.
func modified(n fs.Node) string { return n.MTime.UTC().Format(time.RFC3339Nano) }

type fileInfo struct {
	BaseFileName     string
	OwnerId          string
	Size             int64
	UserId           string
	UserFriendlyName string
	Version          string
	LastModifiedTime string

	UserCanWrite            bool
	UserCanRename           bool
	UserCanNotWriteRelative bool
	ReadOnly                bool

	SupportsLocks              bool
	SupportsGetLock            bool
	SupportsExtendedLockLength bool
	SupportsUpdate             bool
	SupportsRename             bool

	// Where the frame may post its messages to.
	PostMessageOrigin string
	// The editor's close button posts UI_Close to the web UI. Collabora
	// shows it for closebutton=1 alone; OnlyOffice only with this.
	ClosePostMessage bool
}

func (h *Handlers) checkFileInfo(w http.ResponseWriter, r *http.Request, g grant) {
	n, err := h.FS.Get(r.Context(), g.UserID, g.NodeID)
	if err != nil {
		w.WriteHeader(status(r.Context(), err))
		return
	}
	if n.Kind != fs.KindFile {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	canWrite := n.Perms.Has(access.Write)
	// Renaming and saving beside it change the folder, which a file
	// shared on its own doesn't let the user see.
	folderWritable := false
	if dir, err := h.FS.Get(r.Context(), g.UserID, n.ParentID); err == nil {
		folderWritable = dir.Perms.Has(access.Write)
	}
	httpx.WriteJSON(w, http.StatusOK, fileInfo{
		BaseFileName:     n.Name,
		OwnerId:          n.OwnerID,
		Size:             n.Size,
		UserId:           g.UserID,
		UserFriendlyName: g.DisplayName,
		Version:          n.ETag,
		LastModifiedTime: modified(n),

		UserCanWrite:            canWrite,
		UserCanRename:           canWrite && folderWritable,
		UserCanNotWriteRelative: !folderWritable,
		ReadOnly:                !canWrite,

		SupportsLocks:              true,
		SupportsGetLock:            true,
		SupportsExtendedLockLength: true,
		SupportsUpdate:             true,
		SupportsRename:             true,

		PostMessageOrigin: h.PublicOrigin,
		ClosePostMessage:  true,
	})
}

func (h *Handlers) getFile(w http.ResponseWriter, r *http.Request, g grant) {
	rc, n, err := h.FS.Open(r.Context(), g.UserID, g.NodeID, nil)
	if err != nil {
		w.WriteHeader(status(r.Context(), err))
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-WOPI-ItemVersion", n.ETag)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		slog.DebugContext(r.Context(), "wopi: GetFile interrupted", "err", err)
		panic(http.ErrAbortHandler)
	}
}

// conflict answers a lock mismatch: the lock in the way, "" if there is
// none or it isn't a WOPI lock.
func conflict(w http.ResponseWriter, current, reason string) {
	w.Header().Set("X-WOPI-Lock", current)
	w.Header().Set("X-WOPI-LockFailureReason", reason)
	w.WriteHeader(http.StatusConflict)
}

func (h *Handlers) putFile(w http.ResponseWriter, r *http.Request, g grant) {
	if r.Header.Get("X-WOPI-Override") != "PUT" {
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	ctx := r.Context()
	current, locked, err := h.FS.WOPILock(ctx, g.UserID, g.NodeID)
	if err != nil {
		w.WriteHeader(status(ctx, err))
		return
	}
	lock := r.Header.Get("X-WOPI-Lock")
	// Locked by someone else, or another way: refused. Not locked at
	// all, the save goes through: Collabora saves without a lock when the
	// document wasn't locked to begin with.
	if locked && (current == "" || current != lock) {
		conflict(w, current, "locked by another session")
		return
	}
	if locked {
		ctx = fs.WithLockTokens(ctx, []string{fs.WOPILockToken(g.NodeID, current)})
	}
	// Collabora's check that nobody changed the file since it was opened.
	if ts := r.Header.Get("X-COOL-WOPI-Timestamp"); ts != "" {
		n, err := h.FS.Get(ctx, g.UserID, g.NodeID)
		if err != nil {
			w.WriteHeader(status(ctx, err))
			return
		}
		if ts != modified(n) {
			httpx.WriteJSON(w, http.StatusConflict, map[string]int{"COOLStatusCode": 1010})
			return
		}
	}
	n, err := h.FS.WriteFile(ctx, g.UserID, g.NodeID, r.Body, r.ContentLength, fs.PutCond{})
	if errors.Is(err, fs.ErrLocked) {
		conflict(w, "", "locked")
		return
	}
	if err != nil {
		w.WriteHeader(status(ctx, err))
		return
	}
	w.Header().Set("X-WOPI-ItemVersion", n.ETag)
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"LastModifiedTime": modified(n)})
}

func (h *Handlers) override(w http.ResponseWriter, r *http.Request, g grant) {
	ctx := r.Context()
	lock := r.Header.Get("X-WOPI-Lock")
	var current string
	var err error
	switch r.Header.Get("X-WOPI-Override") {
	case "LOCK":
		current, err = h.FS.LockWOPI(ctx, g.UserID, g.NodeID, lock, r.Header.Get("X-WOPI-OldLock"))
	case "REFRESH_LOCK":
		current, err = h.FS.RefreshWOPILock(ctx, g.UserID, g.NodeID, lock)
	case "UNLOCK":
		current, err = h.FS.UnlockWOPI(ctx, g.UserID, g.NodeID, lock)
	case "GET_LOCK":
		current, _, err = h.FS.WOPILock(ctx, g.UserID, g.NodeID)
		if err == nil {
			w.Header().Set("X-WOPI-Lock", current)
		}
	case "PUT_RELATIVE":
		h.putRelative(w, r, g)
		return
	case "RENAME_FILE":
		h.rename(w, r, g)
		return
	default:
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	switch {
	case errors.Is(err, fs.ErrLocked):
		conflict(w, current, "lock mismatch")
	case err != nil:
		w.WriteHeader(status(ctx, err))
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// putRelative saves the document as a new file beside the current one
// ("Save as", or an export), and answers with how to open it.
func (h *Handlers) putRelative(w http.ResponseWriter, r *http.Request, g grant) {
	ctx := r.Context()
	suggested, relative := r.Header.Get("X-WOPI-SuggestedTarget"), r.Header.Get("X-WOPI-RelativeTarget")
	if (suggested == "") == (relative == "") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	src, err := h.FS.Get(ctx, g.UserID, g.NodeID)
	if err != nil {
		w.WriteHeader(status(ctx, err))
		return
	}
	var name string
	var cond fs.PutCond
	if suggested != "" {
		if name, err = decodeUTF7(suggested); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Only an extension: the current name with it.
		if strings.HasPrefix(name, ".") {
			name = strings.TrimSuffix(src.Name, path.Ext(src.Name)) + name
		}
		cond.Rename = true
	} else {
		if name, err = decodeUTF7(relative); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !strings.EqualFold(r.Header.Get("X-WOPI-OverwriteRelativeTarget"), "true") {
			cond.IfNoneMatch = []string{"*"}
		}
	}
	n, _, err := h.FS.PutFile(ctx, g.UserID, src.ParentID, name, r.Body, r.ContentLength, cond)
	switch {
	case errors.Is(err, fs.ErrPrecondition), errors.Is(err, fs.ErrNameConflict):
		w.WriteHeader(http.StatusConflict)
		return
	case errors.Is(err, fs.ErrInvalidName):
		w.WriteHeader(http.StatusBadRequest)
		return
	// No way into the folder: saving beside isn't for this user, as
	// CheckFileInfo said.
	case errors.Is(err, fs.ErrNotFound), errors.Is(err, fs.ErrForbidden):
		w.WriteHeader(http.StatusNotImplemented)
		return
	case errors.Is(err, fs.ErrLocked):
		conflict(w, "", "locked")
		return
	case err != nil:
		w.WriteHeader(status(ctx, err))
		return
	}
	token, _, err := h.Tokens.Issue(ctx, g.UserID, n.ID)
	if err != nil {
		w.WriteHeader(status(ctx, err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"Name": n.Name,
		"Url":  h.HostURL + "/wopi/files/" + n.ID + "?access_token=" + url.QueryEscape(token),
	})
}

// rename gives the file a new name, keeping its extension.
func (h *Handlers) rename(w http.ResponseWriter, r *http.Request, g grant) {
	ctx := r.Context()
	base, err := decodeUTF7(r.Header.Get("X-WOPI-RequestedName"))
	if err != nil || base == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	n, err := h.FS.Get(ctx, g.UserID, g.NodeID)
	if err != nil {
		w.WriteHeader(status(ctx, err))
		return
	}
	if lock := r.Header.Get("X-WOPI-Lock"); lock != "" {
		ctx = fs.WithLockTokens(ctx, []string{fs.WOPILockToken(g.NodeID, lock)})
	}
	renamed, err := h.FS.Rename(ctx, g.UserID, g.NodeID, base+path.Ext(n.Name))
	switch {
	case errors.Is(err, fs.ErrLocked):
		current, _, _ := h.FS.WOPILock(r.Context(), g.UserID, g.NodeID)
		conflict(w, current, "locked")
		return
	case errors.Is(err, fs.ErrInvalidName), errors.Is(err, fs.ErrNameConflict):
		w.Header().Set("X-WOPI-InvalidFileNameError", "the name is taken or not allowed")
		w.WriteHeader(http.StatusBadRequest)
		return
	case err != nil:
		w.WriteHeader(status(ctx, err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"Name": strings.TrimSuffix(renamed.Name, path.Ext(renamed.Name))})
}

// editorInfo tells the web UI which files the editor opens: by extension,
// "edit" or "view". Nothing when the editor is unavailable.
func (h *Handlers) editorInfo(w http.ResponseWriter, r *http.Request) {
	exts := map[string]string{}
	if e, err := h.Discovery.Editor(r.Context()); err != nil {
		slog.WarnContext(r.Context(), "wopi: editor unavailable", "err", err)
	} else {
		for ext, a := range e.Actions {
			exts[ext] = "view"
			if a.Edit {
				exts[ext] = "edit"
			}
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"extensions": exts})
}

type editResponse struct {
	URL         string `json:"url"`
	AccessToken string `json:"accessToken"`
	// AccessTokenTTL is when the token expires, in ms since the epoch:
	// what the editor's form field takes.
	AccessTokenTTL int64 `json:"accessTokenTtl"`
}

// edit opens a file in the editor: a token for the user and the editor URL
// for the file, which the web UI posts the token to.
func (h *Handlers) edit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := auth.UserFromContext(ctx)
	n, err := h.FS.Get(ctx, u.ID, r.PathValue("id"))
	switch {
	case errors.Is(err, fs.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such file")
		return
	case err != nil:
		slog.ErrorContext(ctx, "wopi: edit", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	case n.Kind != fs.KindFile:
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeNotAFile, "not a file")
		return
	}
	e, err := h.Discovery.Editor(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "wopi: discovery", "err", err)
		httpx.WriteError(w, http.StatusServiceUnavailable, httpx.CodeEditorUnavailable, "the online editor is unavailable")
		return
	}
	action, ok := e.Actions[strings.TrimPrefix(strings.ToLower(path.Ext(n.Name)), ".")]
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeNotEditable, "the editor doesn't open this type of file")
		return
	}
	token, expires, err := h.Tokens.Issue(ctx, u.ID, n.ID)
	if err != nil {
		slog.ErrorContext(ctx, "wopi: issue token", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
		return
	}
	src := h.HostURL + "/wopi/files/" + n.ID
	editorURL := action.URL
	if !strings.HasSuffix(editorURL, "?") && !strings.HasSuffix(editorURL, "&") {
		if strings.Contains(editorURL, "?") {
			editorURL += "&"
		} else {
			editorURL += "?"
		}
	}
	editorURL += "WOPISrc=" + url.QueryEscape(src) + "&lang=ru"
	httpx.WriteJSON(w, http.StatusOK, editResponse{URL: editorURL, AccessToken: token, AccessTokenTTL: expires.UnixMilli()})
}

// createDocument makes an empty document in a folder ("root" for the
// user's own) for the editor to open: given an empty file, Collabora
// starts a new document of its type. A taken name gets "(2)" and on.
func (h *Handlers) createDocument(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := auth.UserFromContext(ctx)
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, `expected {"name"}`)
		return
	}
	e, err := h.Discovery.Editor(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "wopi: discovery", "err", err)
		httpx.WriteError(w, http.StatusServiceUnavailable, httpx.CodeEditorUnavailable, "the online editor is unavailable")
		return
	}
	if a, ok := e.Actions[strings.TrimPrefix(strings.ToLower(path.Ext(req.Name)), ".")]; !ok || !a.Edit {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeNotEditable, "the editor doesn't make this type of file")
		return
	}
	dir := r.PathValue("id")
	if dir == "root" {
		root, err := h.FS.Root(ctx, u.ID)
		if err != nil {
			slog.ErrorContext(ctx, "wopi: root", "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
			return
		}
		dir = root.ID
	}
	n, _, err := h.FS.PutFile(ctx, u.ID, dir, req.Name, strings.NewReader(""), 0, fs.PutCond{Rename: true})
	switch {
	case errors.Is(err, fs.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such folder")
	case errors.Is(err, fs.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "your rights on this don't allow it")
	case errors.Is(err, fs.ErrNotDir):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeNotADirectory, "not a folder")
	case errors.Is(err, fs.ErrInvalidName):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidName, "name is empty, too long, or contains / or control characters")
	case errors.Is(err, fs.ErrLocked):
		httpx.WriteError(w, http.StatusLocked, httpx.CodeLocked, "the folder is locked")
	case errors.Is(err, fs.ErrQuotaExceeded):
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, httpx.CodeQuotaExceeded, "not enough quota left")
	case err != nil:
		slog.ErrorContext(ctx, "wopi: create document", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
	default:
		httpx.WriteJSON(w, http.StatusCreated, map[string]string{"id": n.ID, "name": n.Name})
	}
}
