// Package admin is the administrator's API (R15): accounts, the audit log
// and what the system runs with. Groups are share's (/api/v1/admin/groups).
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/riverqueue/river"

	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/httpx"
	"github.com/do-service/cloud/backend/internal/version"
)

const (
	pageSize = 100
	maxBody  = 4 << 10
)

// System is what the system page shows: settings that come from the
// environment, read-only here.
type System struct {
	PasswordLogin bool
	// Providers lists the enabled sign-in providers.
	Providers func(ctx context.Context) ([]auth.Provider, error)
	// Editor reports whether an online editor is configured and answers.
	Editor            func(ctx context.Context) (configured bool, err error)
	EditorURL         string
	DefaultQuotaBytes *int64
	AuditRetention    time.Duration
	TrashRetention    time.Duration
}

type Handlers struct {
	Users *auth.Users
	Audit *audit.Log
	// Devices enables a user's devices, listed and revoked; nil leaves
	// them out.
	Devices *auth.Devices
	// SetUp prepares a new account (its root folder).
	SetUp func(ctx context.Context, userID string) error
	// Enqueue schedules the removal of a user marked for deletion.
	Enqueue  func(ctx context.Context, userID string) error
	System   System
	SignedIn func(http.Handler) http.Handler
}

func (h *Handlers) Register(mux *http.ServeMux) {
	admin := func(f http.HandlerFunc) http.Handler { return h.SignedIn(adminOnly(f)) }
	body := httpx.LimitBody(maxBody)
	mux.Handle("GET /api/v1/admin/users", admin(h.listUsers))
	mux.Handle("POST /api/v1/admin/users", body(admin(h.createUser)))
	mux.Handle("PATCH /api/v1/admin/users/{id}", body(admin(h.updateUser)))
	mux.Handle("DELETE /api/v1/admin/users/{id}", admin(h.deleteUser))
	if h.Devices != nil {
		mux.Handle("GET /api/v1/admin/users/{id}/devices", admin(h.listDevices))
		mux.Handle("DELETE /api/v1/admin/users/{id}/devices/{device}", admin(h.revokeDevice))
	}
	mux.Handle("GET /api/v1/admin/audit", admin(h.auditLog))
	mux.Handle("GET /api/v1/admin/system", admin(h.system))
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

type accountJSON struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"displayName"`
	Email       string     `json:"email"`
	IsAdmin     bool       `json:"isAdmin"`
	Status      string     `json:"status"`
	QuotaBytes  *int64     `json:"quotaBytes"`
	UsedBytes   int64      `json:"usedBytes"`
	HasPassword bool       `json:"hasPassword"`
	Providers   []string   `json:"providers"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastSeenAt  *time.Time `json:"lastSeenAt"`
}

func toJSON(a auth.Account) accountJSON {
	providers := a.Providers
	if providers == nil {
		providers = []string{}
	}
	return accountJSON{
		ID: a.ID, Username: a.Username, DisplayName: a.DisplayName, Email: a.Email, IsAdmin: a.IsAdmin, Status: a.Status,
		QuotaBytes: a.QuotaBytes, UsedBytes: a.UsedBytes, HasPassword: a.HasPassword, Providers: providers,
		CreatedAt: a.CreatedAt, LastSeenAt: a.LastSeenAt,
	}
}

func (h *Handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	for _, m := range []struct {
		err    error
		status int
		code   string
		msg    string
	}{
		{auth.ErrNoUser, http.StatusNotFound, httpx.CodeNotFound, "no such user"},
		{auth.ErrUsernameTaken, http.StatusConflict, httpx.CodeUsernameTaken, "username already taken"},
		{auth.ErrInvalidUsername, http.StatusBadRequest, httpx.CodeInvalidUsername, "invalid username or name"},
		{auth.ErrWeakPassword, http.StatusBadRequest, httpx.CodeWeakPassword, "password must be 8 to 1024 bytes"},
		{auth.ErrLastAdmin, http.StatusConflict, httpx.CodeLastAdmin, "the last active administrator must stay one"},
		{auth.ErrInvalidStatus, http.StatusBadRequest, httpx.CodeInvalidRequest, "status must be active or disabled"},
	} {
		if errors.Is(err, m.err) {
			httpx.WriteError(w, m.status, m.code, m.msg)
			return
		}
	}
	slog.ErrorContext(r.Context(), "admin: request failed", "err", err)
	httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal server error")
}

func (h *Handlers) listUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, next, err := h.Users.Accounts(r.Context(), q.Get("q"), q.Get("cursor"), pageSize)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]accountJSON, len(list))
	for i, a := range list {
		items[i] = toJSON(a)
	}
	var cursor *string
	if next != "" {
		cursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": cursor})
}

// decodeFields reads a JSON object keeping which fields are there: in a
// change, a field set to null (an unlimited quota) differs from one left
// out.
func decodeFields(r *http.Request) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

// quotaField reads quotaBytes: null for unlimited, else a byte count.
func quotaField(raw json.RawMessage) (*int64, bool) {
	if string(raw) == "null" {
		return nil, true
	}
	var q int64
	if err := json.Unmarshal(raw, &q); err != nil || q < 0 {
		return nil, false
	}
	return &q, true
}

func (h *Handlers) createUser(w http.ResponseWriter, r *http.Request) {
	fields, ok := decodeFields(r)
	var req struct {
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
		Password    string `json:"password"`
		IsAdmin     bool   `json:"isAdmin"`
	}
	if ok {
		raw, _ := json.Marshal(fields)
		ok = json.Unmarshal(raw, &req) == nil
	}
	var quota **int64
	if raw, set := fields["quotaBytes"]; ok && set {
		q, valid := quotaField(raw)
		ok = valid
		quota = &q
	}
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, `expected {"username", "password", "displayName", "isAdmin", "quotaBytes"}`)
		return
	}
	ctx := r.Context()
	u, err := h.Users.CreateLocal(ctx, strings.TrimSpace(req.Username), strings.TrimSpace(req.DisplayName), req.Password, req.IsAdmin)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.SetUp(ctx, u.ID); err != nil {
		h.fail(w, r, err)
		return
	}
	a, err := h.Users.UpdateAccount(ctx, u.ID, auth.AccountChange{Quota: quota})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.Audit.Record(r, audit.Event{Actor: auth.Actor(ctx), Action: audit.UserCreate, TargetID: a.ID, TargetName: a.Username,
		Details: map[string]any{"isAdmin": a.IsAdmin, "quotaBytes": a.QuotaBytes}})
	httpx.WriteJSON(w, http.StatusCreated, toJSON(a))
}

func (h *Handlers) updateUser(w http.ResponseWriter, r *http.Request) {
	fields, ok := decodeFields(r)
	var c auth.AccountChange
	details := map[string]any{}
	for name, raw := range fields {
		var err error
		switch name {
		case "displayName":
			err = json.Unmarshal(raw, &c.DisplayName)
			details[name] = c.DisplayName
		case "isAdmin":
			err = json.Unmarshal(raw, &c.IsAdmin)
			details[name] = c.IsAdmin
		case "status":
			err = json.Unmarshal(raw, &c.Status)
			details[name] = c.Status
		case "password":
			err = json.Unmarshal(raw, &c.Password)
			// Never the password itself.
			details[name] = "changed"
		case "quotaBytes":
			q, valid := quotaField(raw)
			if !valid {
				err = errors.New("bad quota")
			}
			c.Quota = &q
			details[name] = q
		default:
			err = errors.New("unknown field")
		}
		if err != nil {
			ok = false
		}
	}
	if !ok || len(fields) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, `expected some of {"displayName", "isAdmin", "status", "quotaBytes", "password"}`)
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	// Whoever demotes, disables or deletes themselves can't undo it.
	if me := auth.Actor(ctx); me.ID == id && (c.IsAdmin != nil && !*c.IsAdmin || c.Status != nil && *c.Status != auth.StatusActive) {
		httpx.WriteError(w, http.StatusConflict, httpx.CodeOwnAccount, "ask another administrator to do this to your account")
		return
	}
	a, err := h.Users.UpdateAccount(ctx, id, c)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.Audit.Record(r, audit.Event{Actor: auth.Actor(ctx), Action: audit.UserUpdate, TargetID: a.ID, TargetName: a.Username, Details: details})
	httpx.WriteJSON(w, http.StatusOK, toJSON(a))
}

// deleteUser signs the user out for good at once; their files go in the
// background.
func (h *Handlers) deleteUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if auth.Actor(ctx).ID == id {
		httpx.WriteError(w, http.StatusConflict, httpx.CodeOwnAccount, "ask another administrator to do this to your account")
		return
	}
	a, err := h.Users.MarkDeleting(ctx, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.Enqueue(ctx, a.ID); err != nil {
		h.fail(w, r, err)
		return
	}
	h.Audit.Record(r, audit.Event{Actor: auth.Actor(ctx), Action: audit.UserDelete, TargetID: a.ID, TargetName: a.Username,
		Details: map[string]any{"usedBytes": a.UsedBytes}})
	w.WriteHeader(http.StatusAccepted)
}

// listDevices is a user's signed-in sync clients. An unknown user is 404
// rather than an empty list, so a stale page can tell.
func (h *Handlers) listDevices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.Users.Account(ctx, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	list, err := h.Devices.List(ctx, a.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if list == nil {
		list = []auth.Device{}
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

// revokeDevice signs a user's device out at once, as the user can in
// their settings: say, a laptop reported lost.
func (h *Handlers) revokeDevice(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.Users.Account(ctx, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dev, err := h.Devices.Delete(ctx, a.ID, r.PathValue("device"))
	if errors.Is(err, auth.ErrNoDevice) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such device")
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// The same action as the owner's revoke, so the device's history reads
	// as one; whose it was is in the details.
	h.Audit.Record(r, audit.Event{Actor: auth.Actor(ctx), Action: audit.DeviceRevoke, TargetID: dev.ID, TargetName: dev.Name,
		Details: map[string]any{"user": a.Username, "userId": a.ID}})
	w.WriteHeader(http.StatusNoContent)
}

type auditJSON struct {
	ID      int64          `json:"id"`
	At      time.Time      `json:"at"`
	Actor   audit.Actor    `json:"actor"`
	IP      string         `json:"ip"`
	Action  string         `json:"action"`
	Target  audit.Actor    `json:"target"`
	Details map[string]any `json:"details"`
}

// auditSizes are the page sizes the log offers; anything else is a
// mistake rather than a wish for some other size.
var auditSizes = map[int]bool{25: true, 50: true, 100: true, 200: true}

const (
	auditDefaultSize = 50
	maxAuditPage     = 10_000_000
)

// auditFilter reads the log's query; the message says what's wrong.
func auditFilter(q url.Values) (f audit.Filter, page, size int, msg string) {
	page, size = 1, auditDefaultSize
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		// The cap keeps the offset from overflowing.
		if err != nil || n < 1 || n > maxAuditPage {
			return f, 0, 0, "page must be a whole number from 1 to 10000000"
		}
		page = n
	}
	if v := q.Get("size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || !auditSizes[n] {
			return f, 0, 0, "size must be 25, 50, 100 or 200"
		}
		size = n
	}
	f = audit.Filter{Action: q.Get("action"), Actor: q.Get("actor"), Query: q.Get("q")}
	for _, b := range []struct {
		name string
		to   *time.Time
	}{{"from", &f.From}, {"to", &f.To}} {
		if v := q.Get(b.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return f, 0, 0, b.name + " must be an RFC 3339 time"
			}
			*b.to = t
		}
	}
	if v := q.Get("ip"); v != "" {
		if a, err := netip.ParseAddr(v); err == nil {
			f.IP = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		} else if p, err := netip.ParsePrefix(v); err == nil {
			f.IP = p.Masked()
		} else {
			return f, 0, 0, "ip must be an address or a network"
		}
	}
	return f, page, size, ""
}

func (h *Handlers) auditLog(w http.ResponseWriter, r *http.Request) {
	f, page, size, msg := auditFilter(r.URL.Query())
	if msg != "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, msg)
		return
	}
	list, total, err := h.Audit.List(r.Context(), f, (page-1)*size, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]auditJSON, len(list))
	for i, e := range list {
		items[i] = auditJSON{ID: e.ID, At: e.At, Actor: e.Actor, IP: e.IP, Action: e.Action,
			Target: audit.Actor{ID: e.TargetID, Name: e.TargetName}, Details: e.Details}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": page, "size": size})
}

type providerJSON struct {
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	Issuer string `json:"issuer"`
}

func (h *Handlers) system(w http.ResponseWriter, r *http.Request) {
	s := h.System
	providers := []providerJSON{}
	if s.Providers != nil {
		list, err := s.Providers(r.Context())
		if err != nil {
			h.fail(w, r, err)
			return
		}
		for _, p := range list {
			providers = append(providers, providerJSON{Slug: p.Slug, Name: p.Name, Issuer: p.Issuer})
		}
	}
	editor := map[string]any{"configured": false, "available": false, "url": s.EditorURL}
	if s.Editor != nil {
		configured, err := s.Editor(r.Context())
		editor["configured"], editor["available"] = configured, configured && err == nil
	}
	days := func(d time.Duration) int { return int(d / (24 * time.Hour)) }
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"version":            version.Version,
		"passwordLogin":      s.PasswordLogin,
		"providers":          providers,
		"editor":             editor,
		"defaultQuotaBytes":  s.DefaultQuotaBytes,
		"auditRetentionDays": days(s.AuditRetention),
		"trashRetentionDays": days(s.TrashRetention),
	})
}

// DeleteUserArgs is the job removing a user marked for deletion.
type DeleteUserArgs struct {
	UserID string `json:"userId"`
}

func (DeleteUserArgs) Kind() string { return "user_delete" }

// DeleteUserWorker removes the user's files, then the account.
type DeleteUserWorker struct {
	river.WorkerDefaults[DeleteUserArgs]
	FS    *fs.Service
	Users *auth.Users
}

func (w *DeleteUserWorker) Work(ctx context.Context, job *river.Job[DeleteUserArgs]) error {
	if err := w.FS.DeleteOwner(ctx, job.Args.UserID); err != nil {
		return err
	}
	if err := w.Users.Delete(ctx, job.Args.UserID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "user deleted", "user", job.Args.UserID)
	return nil
}

// Timeout: a big account takes a while, one transaction per item.
func (w *DeleteUserWorker) Timeout(*river.Job[DeleteUserArgs]) time.Duration { return 6 * time.Hour }
