package nodes

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/httpx"
	"github.com/do-service/cloud/backend/internal/share"
)

type granteeJSON struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

type shareJSON struct {
	ID          string      `json:"id"`
	Grantee     granteeJSON `json:"grantee"`
	Permissions []string    `json:"permissions"`
	CreatedAt   time.Time   `json:"createdAt"`
}

func toShareJSON(s share.Share) shareJSON {
	return shareJSON{ID: s.ID, Grantee: granteeJSON(s.Grantee), Permissions: permNames(s.Perms), CreatedAt: s.CreatedAt}
}

// listShares returns the shares made on {id} itself.
func (h *Handlers) listShares(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	list, err := h.Shares.List(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := struct {
		Items []shareJSON `json:"items"`
	}{Items: make([]shareJSON, 0, len(list))}
	for _, s := range list {
		resp.Items = append(resp.Items, toShareJSON(s))
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// grant shares {id}: {"granteeType","granteeId","permissions":[...]}. To
// someone it is shared with already, it sets their rights instead.
func (h *Handlers) grant(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	var req struct {
		GranteeType string   `json:"granteeType"`
		GranteeID   string   `json:"granteeId"`
		Permissions []string `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, `expected {"granteeType","granteeId","permissions"}`)
		return
	}
	perms, ok := parsePerms(req.Permissions)
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidPermissions, "permissions are read, write and reshare")
		return
	}
	s, err := h.Shares.Grant(r.Context(), u.ID, r.PathValue("id"), req.GranteeType, req.GranteeID, perms)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.record(r, audit.ShareGrant, s.NodeID, "", shareDetails(s))
	httpx.WriteJSON(w, http.StatusOK, toShareJSON(s))
}

// updateShare changes a share's rights: {"permissions":[...]}.
func (h *Handlers) updateShare(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	var req struct {
		Permissions []string `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, `expected {"permissions"}`)
		return
	}
	perms, ok := parsePerms(req.Permissions)
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidPermissions, "permissions are read, write and reshare")
		return
	}
	s, err := h.Shares.Update(r.Context(), u.ID, r.PathValue("id"), perms)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.record(r, audit.ShareUpdate, s.NodeID, "", shareDetails(s))
	httpx.WriteJSON(w, http.StatusOK, toShareJSON(s))
}

// revoke deletes a share, or lets its recipient leave it.
func (h *Handlers) revoke(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	if err := h.Shares.Revoke(r.Context(), u.ID, r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	h.Audit.Record(r, audit.Event{Actor: auth.Actor(r.Context()), Action: audit.ShareRevoke, TargetID: r.PathValue("id")})
	w.WriteHeader(http.StatusNoContent)
}

type sharedJSON struct {
	nodeJSON
	Owner    granteeJSON `json:"owner"`
	SharedAt time.Time   `json:"sharedAt"`
}

// sharedWithMe lists what others shared with the caller.
func (h *Handlers) sharedWithMe(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	items, err := h.Shares.SharedWithMe(r.Context(), u.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := struct {
		Items []sharedJSON `json:"items"`
	}{Items: make([]sharedJSON, 0, len(items))}
	for _, it := range items {
		j := sharedJSON{nodeJSON: toJSON(it.Node), Owner: granteeJSON(it.Owner), SharedAt: it.SharedAt}
		// Each is the top of what the caller sees of it.
		j.ParentID = nil
		resp.Items = append(resp.Items, j)
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// shareDetails is a share as the audit log keeps it.
func shareDetails(s share.Share) map[string]any {
	return map[string]any{"grantee": s.Grantee.Name, "granteeType": s.Grantee.Type, "permissions": permNames(s.Perms)}
}
