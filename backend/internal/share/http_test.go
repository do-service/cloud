package share

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/do-service/cloud/backend/internal/auth"
)

func TestGroupsAdminOnly(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE users SET is_admin = true WHERE username = 'alice'`); err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewSessions(e.pool, time.Hour)
	session := func(name string) string {
		raw, _, err := sessions.Create(ctx, e.users[name], netip.Addr{}, "")
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	admin, user := session("alice"), session("bob")
	withUser := auth.Middleware(sessions, nil, auth.Cookies{})
	h := &Handlers{Shares: e.shares, SignedIn: func(next http.Handler) http.Handler { return withUser(auth.RequireUser(next)) }}
	mux := http.NewServeMux()
	h.Register(mux)
	do := func(session, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "session", Value: session})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := do(user, "POST", "/api/v1/admin/groups", `{"name":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin creates a group: %d", rec.Code)
	}
	rec := do(admin, "POST", "/api/v1/admin/groups", `{"name":"Дизайн"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := do(admin, "POST", "/api/v1/admin/groups", `{"name":"дизайн"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", rec.Code)
	}
	groups, _ := e.shares.Groups(ctx)
	if rec := do(admin, "PUT", "/api/v1/admin/groups/"+groups[0].ID+"/members/"+e.users["bob"], ""); rec.Code != http.StatusNoContent {
		t.Fatalf("add member: %d %s", rec.Code, rec.Body)
	}
	if rec := do(admin, "GET", "/api/v1/admin/groups", ""); !strings.Contains(rec.Body.String(), `"name":"Bob"`) {
		t.Fatalf("list: %s", rec.Body)
	}
	// Any signed-in user may look people and groups up to share with.
	if rec := do(user, "GET", "/api/v1/directory?q=диз", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"group"`) {
		t.Fatalf("directory: %d %s", rec.Code, rec.Body)
	}
	if rec := do(admin, "DELETE", "/api/v1/admin/groups/"+groups[0].ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
}
