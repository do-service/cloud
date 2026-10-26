package nodes

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

type trashPage struct {
	Items      []trashItemJSON
	NextCursor *string
}

func TestTrashAPI(t *testing.T) {
	e := newEnv(t, nil)
	docs := e.mkdir(t, e.root.ID, "Docs")
	a := e.file(t, docs.ID, "a.txt", "a")
	b := e.file(t, docs.ID, "b.txt", "b")
	c := e.file(t, e.root.ID, "c.txt", "c")

	if rec := e.send(t, "DELETE", "/api/v1/nodes/"+a.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	rec := e.send(t, "POST", "/api/v1/nodes/batch", `{"ops":[{"op":"delete","id":"`+b.ID+`"},{"op":"delete","id":"`+a.ID+`"},{"op":"delete","id":"`+c.ID+`"}]}`)
	results := decode[struct{ Results []batchResult }](t, rec).Results
	if rec.Code != http.StatusOK || results[0].Error != nil || results[0].Node != nil || results[1].Error == nil || results[1].Error.Code != "not_found" || results[2].Error != nil {
		t.Fatalf("batch delete: %d %s", rec.Code, rec.Body)
	}

	rec = e.send(t, "GET", "/api/v1/trash?limit=2", "")
	page := decode[trashPage](t, rec)
	if rec.Code != http.StatusOK || len(page.Items) != 2 || page.NextCursor == nil || page.Items[0].ID != c.ID || page.Items[1].OriginalPath != "Docs" || time.Since(page.Items[0].TrashedAt) > time.Minute {
		t.Fatalf("trash page: %d %s", rec.Code, rec.Body)
	}
	page = decode[trashPage](t, e.send(t, "GET", "/api/v1/trash?limit=2&cursor="+url.QueryEscape(*page.NextCursor), ""))
	if len(page.Items) != 1 || page.Items[0].ID != a.ID || page.NextCursor != nil {
		t.Fatalf("second trash page: %+v", page)
	}

	// The name was taken meanwhile: restored under another one.
	e.file(t, docs.ID, "a.txt", "new")
	rec = e.send(t, "POST", "/api/v1/trash/"+a.ID+"/restore", "")
	if rec.Code != http.StatusOK || decode[nodeJSON](t, rec).Name != "a (2).txt" {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if rec := e.send(t, "DELETE", "/api/v1/trash/"+b.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("purge: %d %s", rec.Code, rec.Body)
	}
	if rec := e.send(t, "DELETE", "/api/v1/trash", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body)
	}
	if page := decode[trashPage](t, e.send(t, "GET", "/api/v1/trash", "")); len(page.Items) != 0 {
		t.Fatalf("trash after emptying: %+v", page)
	}

	for _, tc := range []struct {
		name, method, path string
		code               int
		err                string
	}{
		{"delete root", "DELETE", "/api/v1/nodes/root", 400, "root_immutable"},
		{"delete missing", "DELETE", "/api/v1/nodes/" + b.ID, 404, "not_found"},
		{"restore live", "POST", "/api/v1/trash/" + a.ID + "/restore", 404, "not_found"},
		{"purge bad id", "DELETE", "/api/v1/trash/nope", 404, "not_found"},
		{"bad cursor", "GET", "/api/v1/trash?cursor=zz", 400, "invalid_request"},
		{"bad limit", "GET", "/api/v1/trash?limit=-1", 400, "invalid_request"},
	} {
		rec := e.send(t, tc.method, tc.path, "")
		if rec.Code != tc.code || errorCode(rec) != tc.err {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
	}
}
