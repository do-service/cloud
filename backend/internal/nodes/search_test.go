package nodes

import (
	"net/http"
	"net/url"
	"testing"
)

func TestSearchAPI(t *testing.T) {
	e := newEnv(t, nil)
	docs := e.mkdir(t, e.root.ID, "Docs")
	e.file(t, docs.ID, "Отчёт.txt", "x")
	e.file(t, e.root.ID, "отчёт 2.txt", "y")

	rec := e.send(t, "GET", "/api/v1/search?limit=1&q="+url.QueryEscape("ОТЧЁТ"), "")
	page := decode[struct {
		Items      []searchResultJSON
		NextCursor *string
	}](t, rec)
	if rec.Code != http.StatusOK || len(page.Items) != 1 || page.Items[0].Name != "отчёт 2.txt" || page.Items[0].Path != "" || page.Items[0].ParentID != e.root.ID || page.NextCursor == nil {
		t.Fatalf("first page: %d %s", rec.Code, rec.Body)
	}
	rec = e.send(t, "GET", "/api/v1/search?limit=1&q="+url.QueryEscape("ОТЧЁТ")+"&cursor="+url.QueryEscape(*page.NextCursor), "")
	if page := decode[struct{ Items []searchResultJSON }](t, rec); len(page.Items) != 1 || page.Items[0].Path != "Docs" || page.Items[0].ParentID != docs.ID {
		t.Fatalf("second page: %s", rec.Body)
	}
	if rec := e.send(t, "GET", "/api/v1/search?q=nothing", ""); rec.Code != http.StatusOK || rec.Body.String() != "{\"items\":[],\"nextCursor\":null}\n" {
		t.Fatalf("no match: %d %q", rec.Code, rec.Body)
	}
	for path, code := range map[string]string{
		"/api/v1/search?q=ab":              "query_too_short",
		"/api/v1/search":                   "query_too_short",
		"/api/v1/search?q=abc&limit=x":     "invalid_request",
		"/api/v1/search?q=abc&cursor=junk": "invalid_request",
	} {
		if rec := e.send(t, "GET", path, ""); rec.Code != http.StatusBadRequest || errorCode(rec) != code {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
}
