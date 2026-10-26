package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/httpx"
	"github.com/do-service/cloud/backend/internal/share"
	"github.com/do-service/cloud/backend/internal/storage"
)

// Public links (R10): their owner's API under /api/v1, and the public
// side under /s/{token}/api, which needs no account.

type linkJSON struct {
	ID string `json:"id"`
	// Token is null when it can't be unsealed (SECRET_KEY changed).
	Token       *string    `json:"token"`
	Mode        string     `json:"mode"`
	HasPassword bool       `json:"hasPassword"`
	ExpiresAt   *time.Time `json:"expiresAt"`
	CreatedAt   time.Time  `json:"createdAt"`
}

func toLinkJSON(l share.Link) linkJSON {
	var token *string
	if l.Token != "" {
		token = &l.Token
	}
	return linkJSON{ID: l.ID, Token: token, Mode: l.Mode, HasPassword: l.HasPassword, ExpiresAt: l.ExpiresAt, CreatedAt: l.CreatedAt}
}

func (h *Handlers) registerLinks(mux *http.ServeMux) {
	body := httpx.LimitBody(maxNodeBody)
	mux.Handle("GET /api/v1/nodes/{id}/links", h.SignedIn(http.HandlerFunc(h.listLinks)))
	mux.Handle("POST /api/v1/nodes/{id}/links", body(h.SignedIn(http.HandlerFunc(h.createLink))))
	mux.Handle("PATCH /api/v1/links/{id}", body(h.SignedIn(http.HandlerFunc(h.updateLink))))
	mux.Handle("DELETE /api/v1/links/{id}", h.SignedIn(http.HandlerFunc(h.deleteLink)))

	// The public side: no session, the token is the key.
	mux.Handle("GET /s/{token}/api/info", http.HandlerFunc(h.linkInfo))
	mux.Handle("POST /s/{token}/api/unlock", body(http.HandlerFunc(h.unlockLink)))
	mux.Handle("GET /s/{token}/api/nodes/{id}", h.viaLink(h.publicNode))
	mux.Handle("GET /s/{token}/api/nodes/{id}/children", h.viaLink(h.publicChildren))
	mux.Handle("GET /s/{token}/api/nodes/{id}/content", h.viaLink(h.publicContent))
	mux.Handle("GET /s/{token}/api/nodes/{id}/archive", h.viaLink(h.publicArchive))
	if h.Preview != nil {
		mux.Handle("GET /s/{token}/api/nodes/{id}/preview", h.viaLink(h.publicPreview))
	}
	if h.Uploads != nil {
		mux.Handle("/s/{token}/api/uploads", h.viaLink(h.publicUpload))
		mux.Handle("/s/{token}/api/uploads/", h.viaLink(h.publicUpload))
	}
}

func (h *Handlers) listLinks(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	list, err := h.Links.List(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := struct {
		Items []linkJSON `json:"items"`
	}{Items: make([]linkJSON, 0, len(list))}
	for _, l := range list {
		resp.Items = append(resp.Items, toLinkJSON(l))
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// createLink makes a link: {"mode", "password"?, "expiresAt"?}.
func (h *Handlers) createLink(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	var req struct {
		Mode      string     `json:"mode"`
		Password  string     `json:"password"`
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, `expected {"mode","password"?,"expiresAt"?}`)
		return
	}
	l, err := h.Links.Create(r.Context(), u.ID, r.PathValue("id"), req.Mode, req.Password, req.ExpiresAt)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.record(r, audit.LinkCreate, l.NodeID, "", linkDetails(l))
	httpx.WriteJSON(w, http.StatusCreated, toLinkJSON(l))
}

// updateLink changes what is present of {"mode", "password", "expiresAt"}:
// "password": "" removes the password, "expiresAt": null the expiry.
func (h *Handlers) updateLink(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	var req struct {
		Mode      *string         `json:"mode"`
		Password  *string         `json:"password"`
		ExpiresAt json.RawMessage `json:"expiresAt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, `expected {"mode"?,"password"?,"expiresAt"?}`)
		return
	}
	c := share.LinkChange{Mode: req.Mode, Password: req.Password}
	if req.ExpiresAt != nil {
		var at *time.Time
		if err := json.Unmarshal(req.ExpiresAt, &at); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, "expiresAt must be a time or null")
			return
		}
		c.ExpiresAt = &at
	}
	l, err := h.Links.Update(r.Context(), u.ID, r.PathValue("id"), c)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.record(r, audit.LinkUpdate, l.NodeID, "", linkDetails(l))
	httpx.WriteJSON(w, http.StatusOK, toLinkJSON(l))
}

func (h *Handlers) deleteLink(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	if err := h.Links.Delete(r.Context(), u.ID, r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	h.Audit.Record(r, audit.Event{Actor: auth.Actor(r.Context()), Action: audit.LinkDelete, TargetID: r.PathValue("id")})
	w.WriteHeader(http.StatusNoContent)
}

// linkCookie names the browser's unlock session of a link; Path scopes it
// to that link's URLs.
const linkCookie = "link_session"

func linkPath(r *http.Request) string { return "/s/" + r.PathValue("token") + "/" }

// resolveLink is the live link behind {token}; anything else answers 404,
// whatever the reason, so tokens can't be probed.
func (h *Handlers) resolveLink(w http.ResponseWriter, r *http.Request) (share.Public, bool) {
	p, err := h.Links.Resolve(r.Context(), r.PathValue("token"))
	if errors.Is(err, share.ErrNoLink) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such link")
		return share.Public{}, false
	}
	if err != nil {
		h.fail(w, r, err)
		return share.Public{}, false
	}
	return p, true
}

func (h *Handlers) unlocked(r *http.Request, p share.Public) (bool, error) {
	session := ""
	if c, err := r.Cookie(linkCookie); err == nil {
		session = c.Value
	}
	return h.Links.Unlocked(r.Context(), p, session)
}

type linkInfoJSON struct {
	Name          string     `json:"name"`
	Kind          fs.Kind    `json:"kind"`
	Mode          string     `json:"mode"`
	NeedsPassword bool       `json:"needsPassword"`
	Unlocked      bool       `json:"unlocked"`
	ExpiresAt     *time.Time `json:"expiresAt"`
	// RootID is the link's node: where browsing it starts.
	RootID string `json:"rootId"`
}

// linkInfo says what the link is, before any password.
func (h *Handlers) linkInfo(w http.ResponseWriter, r *http.Request) {
	p, ok := h.resolveLink(w, r)
	if !ok {
		return
	}
	open, err := h.unlocked(r, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, linkInfoJSON{
		Name: p.Name, Kind: p.Kind, Mode: p.Mode, NeedsPassword: p.HasPassword, Unlocked: open, ExpiresAt: p.ExpiresAt, RootID: p.NodeID,
	})
}

// unlockLink checks a link's password: {"password"}. Attempts are limited
// per link and address, as logins are.
func (h *Handlers) unlockLink(w http.ResponseWriter, r *http.Request) {
	p, ok := h.resolveLink(w, r)
	if !ok {
		return
	}
	if h.UnlockLimiter != nil {
		if allowed, retry := h.UnlockLimiter.Allow(p.ID + "|" + auth.ClientIP(r).String()); !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
			httpx.WriteError(w, http.StatusTooManyRequests, httpx.CodeTooManyRequests, "too many attempts, retry later")
			return
		}
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, `expected {"password"}`)
		return
	}
	session, expires, err := h.Links.Unlock(r.Context(), p, req.Password)
	if errors.Is(err, share.ErrBadPassword) {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeInvalidCredentials, "wrong password")
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if session != "" {
		http.SetCookie(w, &http.Cookie{
			Name: linkCookie, Value: session, Path: linkPath(r), Expires: expires,
			HttpOnly: true, Secure: h.SecureCookies, SameSite: http.SameSiteLaxMode,
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

type linkCtxKey struct{}

// viaLink runs next for a live link the browser has unlocked, with the
// link in the request context.
func (h *Handlers) viaLink(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.resolveLink(w, r)
		if !ok {
			return
		}
		open, err := h.unlocked(r, p)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if !open {
			httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "this link needs its password")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), linkCtxKey{}, p)))
	})
}

func linkFrom(r *http.Request) share.Public {
	p, _ := r.Context().Value(linkCtxKey{}).(share.Public)
	return p
}

// linkNodeID is the {id} path value, "root" meaning the link's own node.
func linkNodeID(r *http.Request, p share.Public) string {
	if id := r.PathValue("id"); id != rootAlias {
		return id
	}
	return p.NodeID
}

func (h *Handlers) publicNode(w http.ResponseWriter, r *http.Request) {
	p := linkFrom(r)
	id := linkNodeID(r, p)
	n, err := h.FS.Get(r.Context(), p.Principal(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	path, err := h.FS.Path(r.Context(), p.Principal(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, withPath(n, path))
}

func (h *Handlers) publicChildren(w http.ResponseWriter, r *http.Request) {
	p := linkFrom(r)
	h.listChildren(w, r, p.Principal(), linkNodeID(r, p))
}

// publicContent shows a file of a view link in the browser (safe types
// only), or downloads it through a download link.
func (h *Handlers) publicContent(w http.ResponseWriter, r *http.Request) {
	p := linkFrom(r)
	if p.Mode != share.ModeView && p.Mode != share.ModeDownload {
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "this link only takes uploads")
		return
	}
	view := p.Mode == share.ModeView
	// A download link shows too when asked to (?inline=1): its viewer.
	inline := view || r.URL.Query().Get("inline") == "1"
	h.serve(w, r, inline, func(pick func(fs.Node) (*storage.Range, error)) (io.ReadCloser, fs.Node, error) {
		return h.FS.OpenRange(r.Context(), p.Principal(), linkNodeID(r, p), func(n fs.Node) (*storage.Range, error) {
			// A view link shows; what can't be shown isn't handed out.
			if view && !viewable(n.MIME) {
				return nil, fs.ErrForbidden
			}
			return pick(n)
		})
	})
}

func (h *Handlers) publicArchive(w http.ResponseWriter, r *http.Request) {
	p := linkFrom(r)
	if p.Mode != share.ModeDownload {
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "this link doesn't allow downloading")
		return
	}
	h.streamArchive(w, r, p.Principal(), linkNodeID(r, p))
}

func (h *Handlers) publicUpload(w http.ResponseWriter, r *http.Request) {
	p := linkFrom(r)
	if p.Mode != share.ModeUpload {
		httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "this link doesn't take uploads")
		return
	}
	h.Uploads.ServeLink(w, r, "/s/"+r.PathValue("token")+"/api/uploads", p.Principal(), p.NodeID)
}

// linkDetails is a link as the audit log keeps it: never its token.
func linkDetails(l share.Link) map[string]any {
	return map[string]any{"link": l.ID, "mode": l.Mode, "password": l.HasPassword, "expiresAt": l.ExpiresAt}
}
