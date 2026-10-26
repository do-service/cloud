package engine

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/do-service/cloud/desktop/internal/api"
)

// A folder shared through the real server: its owner shares it with the
// client's user (writable), the client syncs it both ways, the owner
// makes it read-only, then revokes it; the user's unsynced edit survives
// each step.
func TestIntegrationShared(t *testing.T) {
	c := newCloud(t, 0)
	owner := c.srv.CreateUser(t, "owner", 0)
	cookie := c.srv.Session(t, owner)
	oa, err := api.New(c.srv.URL, api.Options{Token: api.StaticToken(c.srv.DeviceToken(t, owner)), Retry: fastRetry,
		HTTPClient: &http.Client{Transport: &http.Transport{Proxy: nil}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	roots, err := oa.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := oa.Mkdir(ctx, roots.Roots[0].ID, "Проект")
	if err != nil {
		t.Fatal(err)
	}
	upload(t, oa, proj.ID, "план.txt", "v1")

	b := c.client("Ноутбук", nil)
	b.write("своё.txt", "own")
	b.sync()

	var sh struct {
		ID string `json:"id"`
	}
	grant := map[string]any{"granteeType": "user", "granteeId": c.user.ID, "permissions": []string{"read", "write"}}
	if err := c.srv.JSON(http.MethodPost, "/api/v1/nodes/"+proj.ID+"/shares", cookie, grant, &sh); err != nil {
		t.Fatal(err)
	}
	// Listed, not synced until checked.
	b.sync()
	if _, err := os.Stat(filepath.Join(b.root, SharedName)); !os.IsNotExist(err) {
		t.Fatalf("a share came before it was checked: %v", err)
	}
	if _, err := b.e.Select(ctx, "/"+SharedName+"/Проект"); err != nil {
		t.Fatal(err)
	}
	b.sync()
	dir := SharedName + "/Проект"
	if b.read(dir+"/план.txt") != "v1" {
		t.Fatal("the share didn't come")
	}

	ownerTree := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		names := map[string]string{proj.ID: ""}
		if _, err := oa.Tree(ctx, proj.ID, func(n api.Node) error {
			if n.ID == proj.ID {
				return nil
			}
			p := names[n.ParentID] + "/" + n.Name
			names[n.ID] = p
			out[strings.TrimPrefix(p, "/")] = n.SHA256
			if n.Kind == api.KindDir {
				out[strings.TrimPrefix(p, "/")] = "/"
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	sharedLocal := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for p, v := range localTree(t, b.root) {
			if rest, ok := strings.CutPrefix(p, dir+"/"); ok {
				out[rest] = v
			}
		}
		return out
	}

	// Both ways: the user's edit and new file go into the owner's tree,
	// the owner's new file comes.
	b.write(dir+"/план.txt", "v2 from B")
	b.write(dir+"/Заметки/b.txt", "b")
	b.sync()
	upload(t, oa, proj.ID, "от владельца.txt", "o")
	b.sync()
	if d := diffTrees(sharedLocal(), ownerTree()); d != "" {
		t.Fatalf("the share differs:\n%s", d)
	}
	if ownerTree()["план.txt"] != shaOf("v2 from B") || b.read(dir+"/от владельца.txt") != "o" {
		t.Fatalf("owner's %v", ownerTree())
	}

	// Read-only now: the edit stays a local conflict copy, the server's
	// version comes back.
	if err := c.srv.JSON(http.MethodPatch, "/api/v1/shares/"+sh.ID, cookie, map[string]any{"permissions": []string{"read"}}, nil); err != nil {
		t.Fatal(err)
	}
	b.write(dir+"/план.txt", "v3 read-only")
	b.sync()
	if b.read(dir+"/план.txt") != "v2 from B" || ownerTree()["план.txt"] != shaOf("v2 from B") {
		t.Fatal("an edit got into a read-only share")
	}
	var roCopy string
	for p := range localTree(t, b.root) {
		if strings.HasPrefix(p, dir+"/план (конфликт, Ноутбук, ") {
			roCopy = p
		}
	}
	if roCopy == "" || b.read(roCopy) != "v3 read-only" {
		t.Fatalf("no conflict copy: %v", localTree(t, b.root))
	}

	// Writable again: the copy goes up.
	if err := c.srv.JSON(http.MethodPatch, "/api/v1/shares/"+sh.ID, cookie, map[string]any{"permissions": []string{"read", "write"}}, nil); err != nil {
		t.Fatal(err)
	}
	b.sync()
	if ownerTree()[strings.TrimPrefix(roCopy, dir+"/")] != shaOf("v3 read-only") {
		t.Fatalf("the copy didn't go up: %v", ownerTree())
	}

	// Revoked with an edit not synced yet: the share leaves the disk,
	// the edit goes to the user's own tree.
	b.write(dir+"/Заметки/b.txt", "unsynced")
	// The stamp must differ from base's even on a coarse clock.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(b.root, dir, "Заметки", "b.txt"), future, future); err != nil {
		t.Fatal(err)
	}
	if err := c.srv.JSON(http.MethodDelete, "/api/v1/shares/"+sh.ID, cookie, nil, nil); err != nil {
		t.Fatal(err)
	}
	b.sync()
	if _, err := os.Stat(filepath.Join(b.root, dir)); !os.IsNotExist(err) {
		t.Fatalf("the revoked share is still on disk: %v\n%s", err, b.dump())
	}
	mine := c.serverTree(b.api)
	var kept []string
	for p, sha := range mine {
		if sha == shaOf("unsynced") {
			kept = append(kept, p)
		}
	}
	if len(kept) != 1 || !strings.HasPrefix(kept[0], "b (конфликт, Ноутбук, ") || mine["своё.txt"] != shaOf("own") {
		t.Fatalf("own tree %v, kept %v", mine, kept)
	}
	if d := diffTrees(localTree(t, b.root), mine); d != "" {
		t.Fatalf("local and server differ:\n%s", d)
	}
	if ownerTree()["Заметки/b.txt"] != shaOf("b") {
		t.Fatal("the owner's file changed")
	}
	if res, err := b.e.Pass(ctx); err != nil || res.Ops != 0 {
		t.Fatalf("extra pass: %+v, %v", res, err)
	}
}
