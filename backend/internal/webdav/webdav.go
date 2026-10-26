// Package webdav serves each user's files over WebDAV (R11) at
// {prefix}/{username}/…, on top of the fs service: every operation goes
// through fs, which checks access, so WebDAV can't skip a check.
//
// The protocol handling started as a fork of golang.org/x/net/webdav
// (docs/webdav-spike.md): its FileSystem interface answers PROPFIND with
// two calls per child and can't express quota, conditional writes or
// file ids. Here a request resolves its path to a node once and acts on
// nodes by id. The If-header parser is the library's (if.go). Locks are
// fs's: kept in Postgres and checked by every write, whatever the
// protocol; a request presents its lock tokens in the If header.
package webdav

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/nodes"
	"github.com/do-service/cloud/backend/internal/share"
	"github.com/do-service/cloud/backend/internal/storage"
)

// Statuses RFC 4918 adds to HTTP.
const (
	statusMulti               = 207
	statusLocked              = 423
	statusFailedDependency    = 424
	statusInsufficientStorage = 507
)

func statusText(code int) string {
	switch code {
	case statusMulti:
		return "Multi-Status"
	case statusLocked:
		return "Locked"
	case statusFailedDependency:
		return "Failed Dependency"
	case statusInsufficientStorage:
		return "Insufficient Storage"
	}
	return http.StatusText(code)
}

var (
	errInvalidDepth       = errors.New("webdav: invalid depth")
	errInvalidDestination = errors.New("webdav: invalid destination")
	errDestEqualsSource   = errors.New("webdav: destination equals source")
	errMethod             = errors.New("webdav: method not allowed here")
)

// Handler is the WebDAV endpoint under one prefix. It expects the user
// in the request context (auth.BasicAuth).
type Handler struct {
	files  *fs.Service
	shares *share.Service
	audit  *audit.Log
	prefix string
}

// New returns a constructor for the endpoint under a prefix ("/dav/files",
// no trailing slash). log records deletions; nil records nothing.
func New(files *fs.Service, shares *share.Service, log *audit.Log) func(prefix string) http.Handler {
	return func(prefix string) http.Handler {
		return &Handler{files: files, shares: shares, audit: log, prefix: prefix}
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var status int
	var err error
	switch r.Method {
	case http.MethodOptions:
		status, err = h.options(w, r)
	case http.MethodGet, http.MethodHead:
		status, err = h.get(w, r)
	case http.MethodPut:
		status, err = h.put(w, r)
	case http.MethodDelete:
		status, err = h.delete(w, r)
	case "MKCOL":
		status, err = h.mkcol(w, r)
	case "COPY", "MOVE":
		status, err = h.copyMove(w, r)
	case "PROPFIND":
		status, err = h.propfind(w, r)
	case "PROPPATCH":
		status, err = h.proppatch(w, r)
	case "LOCK":
		status, err = h.lock(w, r)
	case "UNLOCK":
		status, err = h.unlock(w, r)
	default:
		status, err = http.StatusMethodNotAllowed, errMethod
	}
	if status != 0 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		if status != http.StatusNoContent && status != http.StatusCreated {
			_, _ = io.WriteString(w, statusText(status))
		}
	}
	switch {
	case err == nil:
	case status == http.StatusInternalServerError:
		slog.ErrorContext(r.Context(), "webdav: request failed", "method", r.Method, "status", status, "err", err)
	default:
		// Most are ordinary answers to clients probing for files, and
		// their text carries user paths.
		slog.DebugContext(r.Context(), "webdav: request refused", "method", r.Method, "status", status, "err", err)
	}
}

// fail maps an fs error to its status. conflict is the status of a name
// already taken, which differs by method.
func fail(err error, conflict int) (int, error) {
	switch {
	case errors.Is(err, fs.ErrNotFound):
		return http.StatusNotFound, err
	case errors.Is(err, fs.ErrForbidden), errors.Is(err, fs.ErrIsRoot), errors.Is(err, fs.ErrCrossOwner):
		return http.StatusForbidden, err
	case errors.Is(err, fs.ErrNameConflict):
		return conflict, err
	// A parent that is a file, or a folder moved into itself.
	case errors.Is(err, fs.ErrNotDir), errors.Is(err, fs.ErrCycle):
		return http.StatusConflict, err
	case errors.Is(err, fs.ErrInvalidName), errors.Is(err, fs.ErrSizeMismatch):
		return http.StatusBadRequest, err
	case errors.Is(err, fs.ErrPrecondition):
		return http.StatusPreconditionFailed, err
	case errors.Is(err, fs.ErrLocked):
		return statusLocked, err
	case errors.Is(err, fs.ErrQuotaExceeded), errors.Is(err, fs.ErrCopyTooLarge), errors.Is(err, fs.ErrPropsTooLarge):
		return statusInsufficientStorage, err
	case errors.Is(err, context.Canceled):
		// The client went away; nobody reads the answer.
		return http.StatusBadRequest, err
	}
	return http.StatusInternalServerError, err
}

// target is the resource a request path names: names below the user's
// root, which is where the path resolves from.
type target struct {
	user  auth.User
	root  fs.Node
	names []string
	// base is the URL path of the user's tree, without a trailing slash.
	base string
	// virtual: the first name is the folder of what others shared.
	virtual bool
	view    *sharedView
}

// resolve parses a request path (decoded) into a target in the signed-in
// user's tree. Another user's path is not found: the tree of a user name
// isn't a thing to probe for.
func (h *Handler) resolve(ctx context.Context, urlPath string) (target, int, error) {
	u, ok := auth.UserFromContext(ctx)
	if !ok {
		return target{}, http.StatusUnauthorized, errors.New("webdav: no user")
	}
	rest, ok := strings.CutPrefix(urlPath, h.prefix+"/")
	if !ok {
		return target{}, http.StatusNotFound, fs.ErrNotFound
	}
	username, sub, _ := strings.Cut(rest, "/")
	if !strings.EqualFold(username, u.Username) {
		return target{}, http.StatusNotFound, fs.ErrNotFound
	}
	root, err := h.files.Root(ctx, u.ID)
	if errors.Is(err, fs.ErrNotFound) {
		root, err = h.files.EnsureRoot(ctx, u.ID)
	}
	if err != nil {
		return target{}, http.StatusInternalServerError, err
	}
	t := target{user: u, root: root, base: h.prefix + "/" + username, view: &sharedView{}}
	for name := range strings.SplitSeq(sub, "/") {
		if name != "" {
			t.names = append(t.names, name)
		}
	}
	if len(t.names) > 0 && mayBeShared(t.names[0]) {
		name, err := h.sharedName(ctx, t)
		if err != nil {
			return target{}, http.StatusInternalServerError, err
		}
		t.virtual = nameKey(t.names[0]) == nameKey(name)
	}
	return t, 0, nil
}

// fixed: the virtual folder or one of its entries, which only the shares
// themselves make and unmake.
func (t target) fixed() bool { return t.virtual && len(t.names) <= 2 }

func (t target) isRoot() bool { return len(t.names) == 0 }

func (t target) name() string { return t.names[len(t.names)-1] }

func (t target) parent() []string { return t.names[:len(t.names)-1] }

// href is the escaped URL path of names; a folder's ends with a slash.
func (t target) href(names []string, dir bool) string {
	p := t.base + "/" + strings.Join(names, "/")
	if dir && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return (&url.URL{Path: p}).EscapedPath()
}

// key identifies the resource as the tree compares names: "a.txt" and
// "A.TXT" are one, whichever prefix they come through.
func (t target) key() string {
	keys := make([]string, len(t.names))
	for i, name := range t.names {
		if _, key, err := fs.NormalizeName(name); err == nil {
			keys[i] = key
		} else {
			keys[i] = name
		}
	}
	return "/" + t.user.ID + "/" + strings.Join(keys, "/")
}

func (h *Handler) lookup(ctx context.Context, t target, names []string) (fs.Node, error) {
	if len(names) == 0 {
		return t.root, nil
	}
	if t.virtual {
		return h.sharedLookup(ctx, t, names)
	}
	return h.files.Lookup(ctx, t.user.ID, t.root.ID, names)
}

// parentDir is the folder a new resource at t goes into. RFC 4918 wants
// 409 when it is missing (§9.3.1, §9.7.1).
func (h *Handler) parentDir(ctx context.Context, t target) (fs.Node, int, error) {
	if t.fixed() {
		return fs.Node{}, http.StatusForbidden, fs.ErrForbidden
	}
	dir, err := h.lookup(ctx, t, t.parent())
	if errors.Is(err, fs.ErrNotFound) {
		return fs.Node{}, http.StatusConflict, err
	}
	if err != nil {
		status, err := fail(err, http.StatusConflict)
		return fs.Node{}, status, err
	}
	if dir.Kind != fs.KindDir {
		return fs.Node{}, http.StatusConflict, fs.ErrNotDir
	}
	return dir, 0, nil
}

func (h *Handler) options(w http.ResponseWriter, r *http.Request) (int, error) {
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, COPY, MOVE, PROPFIND, PROPPATCH, LOCK, UNLOCK")
	// Class 2: locking (RFC 4918 §18).
	w.Header().Set("DAV", "1, 2")
	// Makes Windows' Mini-Redirector talk WebDAV rather than FrontPage.
	w.Header().Set("MS-Author-Via", "DAV")
	return http.StatusOK, nil
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) (int, error) {
	t, status, err := h.resolve(r.Context(), r.URL.Path)
	if err != nil {
		return status, err
	}
	n, err := h.lookup(r.Context(), t, t.names)
	if err != nil {
		return fail(err, http.StatusConflict)
	}
	if n.Kind == fs.KindDir {
		return http.StatusMethodNotAllowed, errMethod
	}
	err = nodes.Serve(w, r, false, func(pick func(fs.Node) (*storage.Range, error)) (io.ReadCloser, fs.Node, error) {
		return h.files.OpenRange(r.Context(), t.user.ID, n.ID, pick)
	})
	if err != nil {
		return fail(err, http.StatusConflict)
	}
	return 0, nil
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) (int, error) {
	t, status, err := h.resolve(r.Context(), r.URL.Path)
	if err != nil {
		return status, err
	}
	if t.isRoot() {
		return http.StatusMethodNotAllowed, errMethod
	}
	ctx, status, err := h.conditions(r, t)
	if err != nil {
		return status, err
	}
	dir, status, err := h.parentDir(ctx, t)
	if err != nil {
		return status, err
	}
	cond := fs.PutCond{IfMatch: etags(r.Header.Get("If-Match")), IfNoneMatch: etags(r.Header.Get("If-None-Match"))}
	// -1 when the body is chunked: storage then takes what comes.
	n, created, err := h.files.PutFile(ctx, t.user.ID, dir.ID, t.name(), r.Body, r.ContentLength, cond)
	if err != nil {
		// A folder in the way can't be PUT over.
		return fail(err, http.StatusMethodNotAllowed)
	}
	w.Header().Set("ETag", quote(n.ETag))
	if created {
		return http.StatusCreated, nil
	}
	return http.StatusNoContent, nil
}

// etags reads an If-Match or If-None-Match list into unquoted ETags; weak
// ones count as their strong form (fs ETags are all strong).
func etags(header string) []string {
	var tags []string
	for tag := range strings.SplitSeq(header, ",") {
		tag = strings.TrimPrefix(strings.TrimSpace(tag), "W/")
		if tag != "" {
			tags = append(tags, strings.Trim(tag, `"`))
		}
	}
	return tags
}

func quote(etag string) string { return `"` + etag + `"` }

func (h *Handler) mkcol(w http.ResponseWriter, r *http.Request) (int, error) {
	t, status, err := h.resolve(r.Context(), r.URL.Path)
	if err != nil {
		return status, err
	}
	// A body would describe the collection to make; none is supported.
	if r.ContentLength > 0 || r.ContentLength < 0 && r.Header.Get("Transfer-Encoding") != "" {
		return http.StatusUnsupportedMediaType, nil
	}
	if t.isRoot() {
		return http.StatusMethodNotAllowed, errMethod
	}
	ctx, status, err := h.conditions(r, t)
	if err != nil {
		return status, err
	}
	dir, status, err := h.parentDir(ctx, t)
	if err != nil {
		return status, err
	}
	if _, err := h.files.CreateDir(ctx, t.user.ID, dir.ID, t.name()); err != nil {
		// Something there already: MKCOL only makes new ones.
		return fail(err, http.StatusMethodNotAllowed)
	}
	return http.StatusCreated, nil
}

// delete moves to the trash: from there the web UI restores what a client
// deleted by mistake, or a sync client's misstep.
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) (int, error) {
	t, status, err := h.resolve(r.Context(), r.URL.Path)
	if err != nil {
		return status, err
	}
	if t.isRoot() {
		return http.StatusForbidden, fs.ErrIsRoot
	}
	if t.fixed() {
		return http.StatusForbidden, fs.ErrForbidden
	}
	ctx, status, err := h.conditions(r, t)
	if err != nil {
		return status, err
	}
	n, err := h.lookup(ctx, t, t.names)
	if err != nil {
		return fail(err, http.StatusConflict)
	}
	if err := h.files.Trash(ctx, t.user.ID, n.ID); err != nil {
		return fail(err, http.StatusConflict)
	}
	h.audit.Record(r, audit.Event{Actor: audit.Actor{ID: t.user.ID, Name: t.user.Username}, Action: audit.NodeTrash,
		TargetID: n.ID, TargetName: n.Name, Details: map[string]any{"via": "webdav"}})
	return http.StatusNoContent, nil
}

func (h *Handler) copyMove(w http.ResponseWriter, r *http.Request) (int, error) {
	ctx := r.Context()
	src, status, err := h.resolve(ctx, r.URL.Path)
	if err != nil {
		return status, err
	}
	dst, status, err := h.destination(r)
	if err != nil {
		return status, err
	}
	if src.isRoot() || dst.isRoot() {
		return http.StatusForbidden, fs.ErrIsRoot
	}
	// What's shared is copied out of the virtual folder, not moved out of
	// it or anything put in its place.
	if dst.fixed() || src.fixed() && (r.Method == "MOVE" || len(src.names) == 1) {
		return http.StatusForbidden, fs.ErrForbidden
	}
	if src.key() == dst.key() && slices.Equal(src.names, dst.names) {
		return http.StatusForbidden, errDestEqualsSource
	}
	// Into itself, or over a folder holding the source: replacing the
	// destination would delete the source with it.
	if s, d := src.key()+"/", dst.key()+"/"; s != d && (strings.HasPrefix(s, d) || strings.HasPrefix(d, s)) {
		return http.StatusConflict, fs.ErrCycle
	}
	overwrite := r.Header.Get("Overwrite") != "F"
	move := r.Method == "MOVE"
	depth := r.Header.Get("Depth")
	switch {
	// A MOVE always takes everything (§9.9.2); a COPY everything or the
	// collection alone (§9.8.3).
	case move && depth != "" && depth != "infinity", !move && depth != "" && depth != "infinity" && depth != "0":
		return http.StatusBadRequest, errInvalidDepth
	}

	// fs checks the locks on what changes: a COPY's source doesn't, so a
	// source locked by someone else is copied all the same (§7.5.1).
	ctx, status, err = h.conditions(r, src)
	if err != nil {
		return status, err
	}

	n, err := h.lookup(ctx, src, src.names)
	if err != nil {
		return fail(err, http.StatusConflict)
	}
	dir, status, err := h.parentDir(ctx, dst)
	if err != nil {
		return status, err
	}
	existing, err := h.lookup(ctx, dst, dst.names)
	replaced := err == nil
	switch {
	case err != nil && !errors.Is(err, fs.ErrNotFound):
		return fail(err, http.StatusConflict)
	// Only the case of a name changes: the same node, renamed in place.
	case replaced && existing.ID == n.ID:
		replaced = false
	case replaced && !overwrite:
		return http.StatusPreconditionFailed, fs.ErrNameConflict
	case replaced:
		// To the trash, like DELETE: not atomic with what follows, but a
		// failure leaves the old one restorable rather than lost.
		if err := h.files.Trash(ctx, src.user.ID, existing.ID); err != nil {
			return fail(err, http.StatusConflict)
		}
	}

	switch {
	case move:
		_, err = h.files.Move(ctx, src.user.ID, n.ID, dir.ID, dst.name())
		if errors.Is(err, fs.ErrCrossOwner) {
			// Between the user's files and someone else's: a copy there,
			// then the original to the trash, as a file manager would.
			if _, err = h.files.Copy(ctx, src.user.ID, n.ID, dir.ID, dst.name(), fs.CopyOptions{}); err == nil {
				err = h.files.Trash(ctx, src.user.ID, n.ID)
			}
		}
	case n.Kind == fs.KindDir && depth == "0":
		_, err = h.files.CreateDir(ctx, src.user.ID, dir.ID, dst.name())
	default:
		_, err = h.files.Copy(ctx, src.user.ID, n.ID, dir.ID, dst.name(), fs.CopyOptions{})
	}
	if err != nil {
		return fail(err, http.StatusPreconditionFailed)
	}
	if replaced {
		return http.StatusNoContent, nil
	}
	return http.StatusCreated, nil
}

// destination resolves the Destination header of a COPY or MOVE. Only a
// path in the same user's tree is one: another host or tree is a server
// this one can't write to (§9.8.5).
func (h *Handler) destination(r *http.Request) (target, int, error) {
	hdr := r.Header.Get("Destination")
	if hdr == "" {
		return target{}, http.StatusBadRequest, errInvalidDestination
	}
	u, err := url.Parse(hdr)
	if err != nil {
		return target{}, http.StatusBadRequest, errInvalidDestination
	}
	if u.Host != "" && u.Host != r.Host {
		return target{}, http.StatusBadGateway, errInvalidDestination
	}
	t, status, err := h.resolve(r.Context(), u.Path)
	if errors.Is(err, fs.ErrNotFound) {
		return target{}, http.StatusBadGateway, errInvalidDestination
	}
	return t, status, err
}

func (h *Handler) propfind(w http.ResponseWriter, r *http.Request) (int, error) {
	ctx := r.Context()
	t, status, err := h.resolve(ctx, r.URL.Path)
	if err != nil {
		return status, err
	}
	// Depth infinity, which a missing header means (§9.1), would walk
	// the whole tree. RFC 4918 lets a server refuse it this way, and
	// clients ask for 0 or 1 anyway.
	depth := strings.TrimSpace(r.Header.Get("Depth"))
	switch depth {
	case "0", "1":
	case "", "infinity":
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, xml.Header+`<D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>`)
		return 0, nil
	default:
		return http.StatusBadRequest, errInvalidDepth
	}
	pf, status, err := readPropfind(r.Body)
	if err != nil {
		return status, err
	}
	n, err := h.lookup(ctx, t, t.names)
	if err != nil {
		return fail(err, http.StatusConflict)
	}
	withChildren := depth == "1" && n.Kind == fs.KindDir

	// The virtual folder has neither (nor an id); its entries' own are
	// shown when asked for one by one.
	virtualDir := t.virtual && len(t.names) == 1
	p := &props{files: h.files, ctx: ctx, t: t}
	if pf.wantsDead() && !virtualDir {
		if p.dead, err = h.files.Props(ctx, t.user.ID, n.ID, withChildren); err != nil {
			return fail(err, http.StatusConflict)
		}
	}
	if pf.wants(xml.Name{Space: davNS, Local: "lockdiscovery"}) && !virtualDir {
		if p.locks, err = h.files.Locks(ctx, t.user.ID, n.ID, withChildren); err != nil {
			return fail(err, http.StatusConflict)
		}
	}
	mw := multistatusWriter{w: w}
	if err := mw.write(t.href(t.names, n.Kind == fs.KindDir), p.find(pf, n, t.names)); err != nil {
		return http.StatusInternalServerError, err
	}
	if withChildren && virtualDir {
		entries, err := h.sharedEntries(ctx, t)
		if err != nil {
			slog.ErrorContext(ctx, "webdav: propfind shared listing failed", "err", err)
			panic(http.ErrAbortHandler)
		}
		names := append(slices.Clip(t.names), "")
		for _, e := range entries {
			names[len(names)-1] = e.name
			if err := mw.write(t.href(names, e.node.Kind == fs.KindDir), p.find(pf, e.node, names)); err != nil {
				panic(http.ErrAbortHandler)
			}
		}
	} else if withChildren {
		// One listing for all children, a page at a time: no query per
		// child (the library's N+1), and a 50 000-file folder streams
		// rather than sits in memory.
		names := append(slices.Clip(t.names), "")
		for cursor := ""; ; {
			items, next, err := h.files.List(ctx, t.user.ID, n.ID, fs.ListOptions{Limit: fs.MaxListLimit, Cursor: cursor})
			if err != nil {
				// The 207 is out; only cutting the response short tells
				// the client it is incomplete.
				slog.ErrorContext(ctx, "webdav: propfind listing failed", "err", err)
				panic(http.ErrAbortHandler)
			}
			for _, c := range items {
				names[len(names)-1] = c.Name
				if err := mw.write(t.href(names, c.Kind == fs.KindDir), p.find(pf, c, names)); err != nil {
					panic(http.ErrAbortHandler)
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
		if t.isRoot() {
			if err := h.writeSharedFolder(ctx, &mw, p, pf, t); err != nil {
				slog.ErrorContext(ctx, "webdav: propfind shared folder failed", "err", err)
				panic(http.ErrAbortHandler)
			}
		}
	}
	if err := mw.close(); err != nil {
		panic(http.ErrAbortHandler)
	}
	return 0, nil
}

func (h *Handler) proppatch(w http.ResponseWriter, r *http.Request) (int, error) {
	ctx := r.Context()
	t, status, err := h.resolve(ctx, r.URL.Path)
	if err != nil {
		return status, err
	}
	if t.virtual && len(t.names) == 1 {
		return http.StatusForbidden, fs.ErrForbidden
	}
	ctx, status, err = h.conditions(r, t)
	if err != nil {
		return status, err
	}
	n, err := h.lookup(ctx, t, t.names)
	if err != nil {
		return fail(err, http.StatusConflict)
	}
	patches, status, err := readProppatch(r.Body)
	if err != nil {
		return status, err
	}

	var changes []fs.PropChange
	var names []xml.Name
	protected := false
	for _, p := range patches {
		names = append(names, p.name)
		if _, live := liveProps[p.name]; live {
			protected = true
		}
		changes = append(changes, fs.PropChange{Prop: fs.Prop{Space: p.name.Space, Local: p.name.Local, Value: string(p.value)}, Remove: p.remove})
	}
	// All or nothing (§9.2): one protected property fails the rest too.
	statuses := map[xml.Name]int{}
	switch {
	case protected:
		for _, name := range names {
			statuses[name] = statusFailedDependency
			if _, live := liveProps[name]; live {
				statuses[name] = http.StatusForbidden
			}
		}
	default:
		code := http.StatusOK
		if err := h.files.PatchProps(ctx, t.user.ID, n.ID, changes); err != nil {
			// Refused as a whole, not property by property.
			if errors.Is(err, fs.ErrNotFound) || errors.Is(err, fs.ErrForbidden) || errors.Is(err, fs.ErrLocked) {
				return fail(err, http.StatusConflict)
			}
			if code, err = fail(err, http.StatusConflict); code >= 500 {
				return code, err
			}
		}
		for _, name := range names {
			statuses[name] = code
		}
	}
	mw := multistatusWriter{w: w}
	if err := mw.write(t.href(t.names, n.Kind == fs.KindDir), byStatus(names, statuses)); err != nil {
		return http.StatusInternalServerError, err
	}
	if err := mw.close(); err != nil {
		return http.StatusInternalServerError, err
	}
	return 0, nil
}

// byStatus groups empty properties into one propstat per status.
func byStatus(names []xml.Name, statuses map[xml.Name]int) []propstat {
	var out []propstat
	for _, name := range names {
		code := statuses[name]
		i := slices.IndexFunc(out, func(ps propstat) bool { return ps.status == code })
		if i < 0 {
			out = append(out, propstat{status: code})
			i = len(out) - 1
		}
		out[i].props = append(out[i].props, property{name: name})
	}
	return out
}
