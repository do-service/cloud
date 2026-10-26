package share

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/httpx"
)

// directoryLimit is how many users and how many groups one lookup returns:
// a picker narrows down as the user types.
const directoryLimit = 20

// Handlers serves /api/v1/directory and the admin's /api/v1/admin/groups.
// Shares on nodes are served with the nodes (internal/nodes).
type Handlers struct {
	Shares *Service
	// SignedIn wraps a route so it runs only for a signed-in user.
	SignedIn func(http.Handler) http.Handler
	// Audit records the admin's group changes; nil records nothing.
	Audit *audit.Log
}

func (h *Handlers) record(r *http.Request, action, id, name string, details map[string]any) {
	h.Audit.Record(r, audit.Event{Actor: auth.Actor(r.Context()), Action: action, TargetID: id, TargetName: name, Details: details})
}

func (h *Handlers) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/directory", h.SignedIn(http.HandlerFunc(h.directory)))
	admin := func(f http.HandlerFunc) http.Handler { return h.SignedIn(adminOnly(f)) }
	body := httpx.LimitBody(4 << 10)
	mux.Handle("GET /api/v1/admin/groups", admin(h.groups))
	mux.Handle("POST /api/v1/admin/groups", body(admin(h.createGroup)))
	mux.Handle("PATCH /api/v1/admin/groups/{id}", body(admin(h.renameGroup)))
	mux.Handle("DELETE /api/v1/admin/groups/{id}", admin(h.deleteGroup))
	mux.Handle("PUT /api/v1/admin/groups/{id}/members/{user}", admin(h.setMember(true)))
	mux.Handle("DELETE /api/v1/admin/groups/{id}/members/{user}", admin(h.setMember(false)))
}

// adminOnly answers 403 to anyone but an administrator.
func adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, _ := auth.UserFromContext(r.Context()); !u.IsAdmin {
			httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "administrators only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type granteeJSON struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// directory finds users and groups to share with: ?q=part of a name.
func (h *Handlers) directory(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	list, err := h.Shares.Directory(r.Context(), u.ID, r.URL.Query().Get("q"), directoryLimit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := struct {
		Items []granteeJSON `json:"items"`
	}{Items: make([]granteeJSON, 0, len(list))}
	for _, g := range list {
		resp.Items = append(resp.Items, granteeJSON(g))
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

type groupJSON struct {
	ID      string        `json:"id"`
	Name    string        `json:"name"`
	Members []granteeJSON `json:"members"`
}

func toGroupJSON(g Group) groupJSON {
	members := make([]granteeJSON, 0, len(g.Members))
	for _, m := range g.Members {
		members = append(members, granteeJSON(m))
	}
	return groupJSON{ID: g.ID, Name: g.Name, Members: members}
}

func (h *Handlers) groups(w http.ResponseWriter, r *http.Request) {
	list, err := h.Shares.Groups(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := struct {
		Items []groupJSON `json:"items"`
	}{Items: make([]groupJSON, 0, len(list))}
	for _, g := range list {
		resp.Items = append(resp.Items, toGroupJSON(g))
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func decodeName(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, `expected {"name"}`)
		return "", false
	}
	return req.Name, true
}

func (h *Handlers) createGroup(w http.ResponseWriter, r *http.Request) {
	name, ok := decodeName(w, r)
	if !ok {
		return
	}
	g, err := h.Shares.CreateGroup(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.record(r, audit.GroupCreate, g.ID, g.Name, nil)
	httpx.WriteJSON(w, http.StatusCreated, toGroupJSON(g))
}

func (h *Handlers) renameGroup(w http.ResponseWriter, r *http.Request) {
	name, ok := decodeName(w, r)
	if !ok {
		return
	}
	if err := h.Shares.RenameGroup(r.Context(), r.PathValue("id"), name); err != nil {
		h.fail(w, r, err)
		return
	}
	h.record(r, audit.GroupRename, r.PathValue("id"), name, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) deleteGroup(w http.ResponseWriter, r *http.Request) {
	g := h.group(r, r.PathValue("id"))
	if err := h.Shares.DeleteGroup(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	h.record(r, audit.GroupDelete, g.ID, g.Name, nil)
	w.WriteHeader(http.StatusNoContent)
}

// group looks a group up for the audit log, which names what it was done
// to: before a removal, while it's still there. Groups are few, and only
// admins change them.
func (h *Handlers) group(r *http.Request, id string) Group {
	if h.Audit == nil {
		return Group{ID: id}
	}
	groups, err := h.Shares.Groups(r.Context())
	if err == nil {
		for _, g := range groups {
			if g.ID == id {
				return g
			}
		}
	}
	return Group{ID: id}
}

func memberName(g Group, userID string) string {
	for _, m := range g.Members {
		if m.ID == userID {
			return m.Name
		}
	}
	return userID
}

func (h *Handlers) setMember(member bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		action := audit.GroupMember
		if !member {
			action = audit.GroupRemoved
		}
		id, user := r.PathValue("id"), r.PathValue("user")
		// Named while a member: before removing, after adding.
		before := h.group(r, id)
		if err := h.Shares.SetMember(r.Context(), id, user, member); err != nil {
			h.fail(w, r, err)
			return
		}
		g := before
		if member {
			g = h.group(r, id)
		}
		h.record(r, action, id, g.Name, map[string]any{"user": memberName(g, user), "userId": user})
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNoGroup):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such group")
	case errors.Is(err, ErrGroupName):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidName, "group name must be 1 to 100 characters")
	case errors.Is(err, ErrGroupExists):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeNameConflict, "a group with this name exists")
	case errors.Is(err, ErrInvalidGrantee):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidGrantee, "no such user")
	default:
		slog.ErrorContext(r.Context(), "share: request failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
	}
}
