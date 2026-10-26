package nodes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// as serves one request as another signed-in user.
func (e env) as(t *testing.T, session, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: session})
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func TestSharesAPI(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	var bob string
	if err := e.pool.QueryRow(ctx, `INSERT INTO users (username, display_name) VALUES ('bob', 'Боб') RETURNING id`).Scan(&bob); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.EnsureRoot(ctx, bob); err != nil {
		t.Fatal(err)
	}
	bobSession, _, err := e.sessions.Create(ctx, bob, netip.Addr{}, "")
	if err != nil {
		t.Fatal(err)
	}
	docs := e.mkdir(t, e.root.ID, "Docs")
	e.file(t, docs.ID, "a.txt", "a")

	// The owner sees all rights on their nodes.
	if got := decode[nodeWithPath](t, e.send(t, "GET", "/api/v1/nodes/"+docs.ID, "")).Permissions; strings.Join(got, ",") != "read,write,reshare" {
		t.Fatalf("owner permissions %v", got)
	}

	rec := e.send(t, "POST", "/api/v1/nodes/"+docs.ID+"/shares", `{"granteeType":"user","granteeId":"`+bob+`","permissions":["read"]}`)
	sh := decode[shareJSON](t, rec)
	if rec.Code != http.StatusOK || sh.Grantee.Name != "Боб" || strings.Join(sh.Permissions, ",") != "read" {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body)
	}
	if page := decode[struct{ Items []shareJSON }](t, e.send(t, "GET", "/api/v1/nodes/"+docs.ID+"/shares", "")); len(page.Items) != 1 {
		t.Fatalf("list: %+v", page)
	}

	// bob: in "shared with me", can list and read, not write.
	shared := decode[struct{ Items []sharedJSON }](t, e.as(t, bobSession, "GET", "/api/v1/shared-with-me", ""))
	if len(shared.Items) != 1 || shared.Items[0].ID != docs.ID || strings.Join(shared.Items[0].Permissions, ",") != "read" {
		t.Fatalf("shared with bob: %+v", shared)
	}
	rec = e.as(t, bobSession, "GET", "/api/v1/nodes/"+docs.ID+"/children", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"permissions":["read"]`) {
		t.Fatalf("bob lists: %d %s", rec.Code, rec.Body)
	}
	if rec := e.as(t, bobSession, "POST", "/api/v1/nodes/"+docs.ID+"/children", `{"name":"x"}`); rec.Code != http.StatusForbidden || errorCode(rec) != "forbidden" {
		t.Fatalf("bob mkdir: %d %s", rec.Code, rec.Body)
	}
	if rec := e.as(t, bobSession, "GET", "/api/v1/nodes/"+docs.ID+"/shares", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("bob lists shares: %d", rec.Code)
	}

	// Upgraded to write, bob can work in it; moving to his own tree isn't
	// how it's done.
	if rec := e.send(t, "PATCH", "/api/v1/shares/"+sh.ID, `{"permissions":["read","write"]}`); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	rec = e.as(t, bobSession, "POST", "/api/v1/nodes/"+docs.ID+"/children", `{"name":"от Боба"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bob mkdir with write: %d %s", rec.Code, rec.Body)
	}
	sub := decode[nodeJSON](t, rec)
	if rec := e.as(t, bobSession, "PATCH", "/api/v1/nodes/"+sub.ID, `{"parentId":"root"}`); rec.Code != http.StatusConflict || errorCode(rec) != "cross_owner_move" {
		t.Fatalf("bob moves to his root: %d %s", rec.Code, rec.Body)
	}

	for _, tc := range []struct {
		name, body string
		code       int
		err        string
	}{
		{"unknown right", `{"granteeType":"user","granteeId":"` + bob + `","permissions":["admin"]}`, 400, "invalid_permissions"},
		{"no read", `{"granteeType":"user","granteeId":"` + bob + `","permissions":["write"]}`, 400, "invalid_permissions"},
		{"nobody", `{"granteeType":"user","granteeId":"00000000-0000-0000-0000-000000000000","permissions":["read"]}`, 400, "invalid_grantee"},
	} {
		if rec := e.send(t, "POST", "/api/v1/nodes/"+docs.ID+"/shares", tc.body); rec.Code != tc.code || errorCode(rec) != tc.err {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
	}

	// Revoked: bob's next request is a 404.
	if rec := e.send(t, "DELETE", "/api/v1/shares/"+sh.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := e.as(t, bobSession, "GET", "/api/v1/nodes/"+docs.ID+"/children", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("after revoke: %d", rec.Code)
	}
}

func TestListingMarksShared(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	var bob string
	if err := e.pool.QueryRow(ctx, `INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.EnsureRoot(ctx, bob); err != nil {
		t.Fatal(err)
	}
	bobSession, _, err := e.sessions.Create(ctx, bob, netip.Addr{}, "")
	if err != nil {
		t.Fatal(err)
	}
	people := e.mkdir(t, e.root.ID, "People")
	linked := e.mkdir(t, e.root.ID, "Linked")
	e.mkdir(t, e.root.ID, "Plain")
	inner := e.mkdir(t, people.ID, "Inner")
	e.mkdir(t, inner.ID, "Deep")
	grant := func(id string) {
		t.Helper()
		if rec := e.send(t, "POST", "/api/v1/nodes/"+id+"/shares", `{"granteeType":"user","granteeId":"`+bob+`","permissions":["read"]}`); rec.Code != http.StatusOK {
			t.Fatalf("grant: %d %s", rec.Code, rec.Body)
		}
	}
	grant(people.ID)
	grant(inner.ID)
	if rec := e.send(t, "POST", "/api/v1/nodes/"+linked.ID+"/links", `{"mode":"view"}`); rec.Code >= 300 {
		t.Fatalf("link: %d %s", rec.Code, rec.Body)
	}

	marks := func(rec *httptest.ResponseRecorder) map[string]bool {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		got := map[string]bool{}
		for _, n := range decode[childrenResponse](t, rec).Items {
			got[n.Name] = n.Shared
		}
		return got
	}
	if got := marks(e.send(t, "GET", "/api/v1/nodes/root/children", "")); !got["People"] || !got["Linked"] || got["Plain"] {
		t.Fatalf("owner's root: %v", got)
	}
	// Marked where the share is, not below it.
	if got := marks(e.send(t, "GET", "/api/v1/nodes/"+inner.ID+"/children", "")); got["Deep"] {
		t.Fatalf("below a share: %v", got)
	}

	// bob may read People, not manage its shares: he isn't told of Inner's.
	rec := e.as(t, bobSession, "GET", "/api/v1/nodes/"+people.ID+"/children", "")
	if got := marks(rec); got["Inner"] || strings.Contains(rec.Body.String(), `"shared"`) {
		t.Fatalf("recipient told of shares: %s", rec.Body)
	}

	// Reshare on a sibling listed first doesn't tell him of Inner's either.
	extra := e.mkdir(t, people.ID, "Aaa")
	if rec := e.send(t, "POST", "/api/v1/nodes/"+extra.ID+"/shares", `{"granteeType":"user","granteeId":"`+bob+`","permissions":["read","reshare"]}`); rec.Code != http.StatusOK {
		t.Fatalf("grant reshare: %d %s", rec.Code, rec.Body)
	}
	if got := marks(e.as(t, bobSession, "GET", "/api/v1/nodes/"+people.ID+"/children", "")); got["Inner"] {
		t.Fatalf("reshare on a sibling: %v", got)
	}
}
