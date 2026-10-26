package nodes

import (
	"net/http"

	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/httpx"
)

// quotaResponse is what the user's files take, versions and trash
// included, against their quota (R16); QuotaBytes is null when unlimited.
type quotaResponse struct {
	UsedBytes  int64  `json:"usedBytes"`
	QuotaBytes *int64 `json:"quotaBytes"`
}

func (h *Handlers) quota(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	used, quota, err := h.FS.Quota(r.Context(), u.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, quotaResponse{UsedBytes: used, QuotaBytes: quota})
}
