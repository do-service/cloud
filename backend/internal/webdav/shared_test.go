package webdav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/share"
)

const shared = "/Доступно мне"

func TestSharedFolder(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	as := func(user, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
		return e.as(t, user, method, "/dav/files/"+user+path, body, hdr...)
	}
	// Bob shares "Проекты" read-only and "Общая" for editing; Carol has a
	// "Проекты" of her own for Alice's group.
	status(t, as("bob", "MKCOL", "/Проекты", ""), http.StatusCreated)
	status(t, as("bob", "PUT", "/Проекты/план.txt", "план"), http.StatusCreated)
	status(t, as("bob", "MKCOL", "/Общая", ""), http.StatusCreated)
	status(t, as("carol", "MKCOL", "/Проекты", ""), http.StatusCreated)
	grant := func(owner, path, granteeType, granteeID string, p access.Perm) string {
		n := mustLookupAs(t, e, owner, path)
		s, err := e.shares.Grant(ctx, e.ids[owner], n.ID, granteeType, granteeID, p)
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	grant("bob", "Проекты", share.GranteeUser, e.ids["alice"], access.Read)
	grant("bob", "Общая", share.GranteeUser, e.ids["alice"], access.Read|access.Write)
	group, err := e.shares.CreateGroup(ctx, "Отдел")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.shares.SetMember(ctx, group.ID, e.ids["alice"], true); err != nil {
		t.Fatal(err)
	}
	carols := grant("carol", "Проекты", share.GranteeGroup, group.ID, access.Read)

	root := found(t, e.do(t, "PROPFIND", "/", "", "Depth", "1"))
	if _, ok := root[escaped("/dav/files/alice"+shared+"/")]; !ok {
		t.Fatalf("no shared folder in the root: %v", root)
	}
	ask := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop><d:getetag/><oc:permissions/></d:prop></d:propfind>`
	list := found(t, e.do(t, "PROPFIND", shared+"/", ask, "Depth", "1"))
	ro := list[escaped("/dav/files/alice"+shared+"/Проекты (bob)/")]
	rw := list[escaped("/dav/files/alice"+shared+"/Общая/")]
	if ro["permissions"] != "SG" || rw["permissions"] != "SGCKDNV" || len(list) != 4 {
		t.Fatalf("entries: %v", list)
	}
	before := list[escaped("/dav/files/alice"+shared+"/")]["getetag"]

	// Read-only: readable, not writable.
	if body := e.do(t, "GET", shared+"/Проекты (bob)/план.txt", "").Body.String(); body != "план" {
		t.Fatalf("read shared: %q", body)
	}
	status(t, e.do(t, "PUT", shared+"/Проекты (bob)/x.txt", "x"), http.StatusForbidden)
	status(t, e.do(t, "GET", shared+"/Проекты (carol)/", ""), http.StatusMethodNotAllowed)
	// Editable: writes land in Bob's folder, and the virtual folder's
	// ETag tells sync clients to look.
	status(t, e.do(t, "PUT", shared+"/Общая/от алисы.txt", "привет"), http.StatusCreated)
	if body := as("bob", "GET", "/Общая/от алисы.txt", "").Body.String(); body != "привет" {
		t.Fatalf("bob sees %q", body)
	}
	after := found(t, e.do(t, "PROPFIND", shared+"/", ask, "Depth", "0"))[escaped("/dav/files/alice"+shared+"/")]["getetag"]
	if after == before {
		t.Fatal("shared folder's ETag didn't change")
	}

	// The folder and its entries are the shares' to make and unmake.
	status(t, e.do(t, "MKCOL", shared+"/новая", ""), http.StatusForbidden)
	status(t, e.do(t, "PUT", shared+"/файл.txt", "x"), http.StatusForbidden)
	status(t, e.do(t, "PUT", shared, "x"), http.StatusForbidden)
	status(t, e.do(t, "DELETE", shared+"/Общая", ""), http.StatusForbidden)
	status(t, e.do(t, "DELETE", shared, ""), http.StatusForbidden)
	status(t, e.do(t, "MOVE", shared+"/Общая", "", "Destination", "/dav/files/alice/Моя"), http.StatusForbidden)
	status(t, e.do(t, "COPY", shared+"/Проекты (bob)", "", "Destination", "/dav/files/alice/Копия"), http.StatusCreated)
	if body := e.do(t, "GET", "/Копия/план.txt", "").Body.String(); body != "план" {
		t.Fatalf("copied: %q", body)
	}

	// Moving between owners copies, then trashes the original.
	status(t, e.do(t, "PUT", "/моё.txt", "моё"), http.StatusCreated)
	status(t, e.do(t, "MOVE", "/моё.txt", "", "Destination", "/dav/files/alice"+shared+"/Общая/моё.txt"), http.StatusCreated)
	status(t, e.do(t, "GET", "/моё.txt", ""), http.StatusNotFound)
	if body := as("bob", "GET", "/Общая/моё.txt", "").Body.String(); body != "моё" {
		t.Fatalf("moved into the share: %q", body)
	}
	status(t, e.do(t, "MOVE", shared+"/Общая/моё.txt", "", "Destination", "/dav/files/alice/назад.txt"), http.StatusCreated)
	status(t, as("bob", "GET", "/Общая/моё.txt", ""), http.StatusNotFound)

	// A folder of her own by that name keeps it.
	status(t, e.do(t, "MKCOL", shared, ""), http.StatusForbidden)
	if _, err := e.svc.CreateDir(ctx, e.ids["alice"], mustLookupAs(t, e, "alice").ID, "Доступно мне"); err != nil {
		t.Fatal(err)
	}
	status(t, e.do(t, "PUT", shared+"/своё.txt", "x"), http.StatusCreated)
	if got := found(t, e.do(t, "PROPFIND", shared+" (2)/", "", "Depth", "1")); len(got) != 4 {
		t.Fatalf("renamed shared folder: %v", got)
	}

	// Revoked: gone, and the duplicate name needs no suffix anymore.
	if err := e.shares.Revoke(ctx, e.ids["carol"], carols); err != nil {
		t.Fatal(err)
	}
	status(t, e.do(t, "PROPFIND", shared+" (2)/Проекты (carol)/", "", "Depth", "0"), http.StatusNotFound)
	status(t, e.do(t, "PROPFIND", shared+" (2)/Проекты/", "", "Depth", "0"), statusMulti)
}

func mustLookupAs(t *testing.T, e env, user string, names ...string) fs.Node {
	t.Helper()
	root, err := e.svc.Root(context.Background(), e.ids[user])
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		return root
	}
	n, err := e.svc.Lookup(context.Background(), e.ids[user], root.ID, names)
	if err != nil {
		t.Fatalf("lookup %v: %v", names, err)
	}
	return n
}
