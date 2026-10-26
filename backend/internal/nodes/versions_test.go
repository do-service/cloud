package nodes

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/do-service/cloud/backend/internal/fs"
)

// replace gives the file name in the root new content, the way an upload
// with onConflict=replace does.
func (e env) replace(t *testing.T, name, content string) {
	t.Helper()
	ctx := context.Background()
	up, err := e.svc.ReserveUpload(ctx, e.user, fs.Upload{ParentID: e.root.ID, Name: name, Size: int64(len(content)), OnConflict: fs.ConflictReplace})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.dataDir, "uploads", "tus")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, up.ID), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.FinishUpload(ctx, e.user, up.ID, "uploads/tus/"+up.ID); err != nil {
		t.Fatal(err)
	}
}

type versionsPage struct {
	Items         []versionJSON
	CurrentAuthor *authorJSON
}

func TestVersionsAPI(t *testing.T) {
	e := newEnv(t, nil)
	f := e.file(t, e.root.ID, "a.txt", "first")
	e.replace(t, "a.txt", "second!")

	rec := e.send(t, "GET", "/api/v1/nodes/"+f.ID+"/versions", "")
	page := decode[versionsPage](t, rec)
	if rec.Code != http.StatusOK || len(page.Items) != 1 || page.Items[0].Size != 5 || page.Items[0].CreatedAt.IsZero() {
		t.Fatalf("versions: %d %s", rec.Code, rec.Body)
	}
	// Both contents are the test user's.
	if a := page.CurrentAuthor; a == nil || a.ID != e.user || a.DisplayName == "" || page.Items[0].Author == nil || *page.Items[0].Author != *a {
		t.Fatalf("authors: %s", rec.Body)
	}
	v := page.Items[0].ID

	rec, _ = e.do(t, "GET", "/api/v1/versions/"+v+"/content", map[string]string{"Range": "bytes=1-3"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "irs" || rec.Header().Get("ETag") != `"`+v+`"` ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), `filename="a.txt"`) {
		t.Fatalf("version content: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	if rec, _ := e.do(t, "GET", "/api/v1/versions/"+v+"/content", map[string]string{"If-None-Match": `"` + v + `"`}); rec.Code != http.StatusNotModified {
		t.Fatalf("version 304: %d", rec.Code)
	}

	rec = e.send(t, "POST", "/api/v1/versions/"+v+"/restore", "")
	if rec.Code != http.StatusOK || decode[nodeJSON](t, rec).Size != 5 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := e.do(t, "GET", "/api/v1/nodes/"+f.ID+"/content", nil); rec.Body.String() != "first" {
		t.Fatalf("after restore: %q", rec.Body)
	}
	if page := decode[versionsPage](t, e.send(t, "GET", "/api/v1/nodes/"+f.ID+"/versions", "")); len(page.Items) != 2 {
		t.Fatalf("history after restore: %+v", page)
	}

	for _, tc := range []struct {
		name, method, path string
		code               int
		err                string
	}{
		{"versions of a folder", "GET", "/api/v1/nodes/" + e.root.ID + "/versions", 400, "not_a_file"},
		{"missing version", "GET", "/api/v1/versions/00000000-0000-0000-0000-000000000000/content", 404, "not_found"},
		{"bad version id", "POST", "/api/v1/versions/nope/restore", 404, "not_found"},
	} {
		rec := e.send(t, tc.method, tc.path, "")
		if rec.Code != tc.code || errorCode(rec) != tc.err {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
	}
}
