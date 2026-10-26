package nodes

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/storage"
)

// do serves one request as alice; aborted reports a deliberately cut
// response (http.ErrAbortHandler), which a real server turns into a
// broken connection.
func (e env) do(t *testing.T, method, path string, header map[string]string) (rec *httptest.ResponseRecorder, aborted bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: e.session})
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec = httptest.NewRecorder()
	defer func() {
		if v := recover(); v != nil {
			if v != http.ErrAbortHandler {
				panic(v)
			}
			aborted = true
		}
	}()
	e.mux.ServeHTTP(rec, req)
	return rec, false
}

func (e env) file(t *testing.T, parent, name, content string) fs.Node {
	t.Helper()
	n, err := e.svc.CreateFile(context.Background(), e.user, parent, name, strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return n
}

func (e env) mkdir(t *testing.T, parent, name string) fs.Node {
	t.Helper()
	n, err := e.svc.CreateDir(context.Background(), e.user, parent, name)
	if err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	return n
}

func TestParseRange(t *testing.T) {
	const size = 10
	for _, tc := range []struct {
		header        string
		offset, len   int64 // len 0: no range, serve it all
		unsatisfiable bool
	}{
		{"bytes=2-4", 2, 3, false},
		{"bytes=7-", 7, 3, false},
		{"bytes=5-100", 5, 5, false},
		{"bytes=-3", 7, 3, false},
		{"bytes=-30", 0, 10, false},
		{"bytes=0-0", 0, 1, false},
		{"bytes=10-", 0, 0, true},
		{"bytes=-0", 0, 0, true},
		{"bytes=0-1,4-5", 0, 0, false},
		{"bytes=4-2", 0, 0, false},
		{"bytes=x-", 0, 0, false},
		{"bytes=-", 0, 0, false},
		{"items=0-1", 0, 0, false},
	} {
		rng, ok := parseRange(tc.header, size)
		switch {
		case ok == tc.unsatisfiable:
			t.Errorf("%s: ok %v", tc.header, ok)
		case tc.len == 0 && rng != nil:
			t.Errorf("%s: range %+v, want none", tc.header, rng)
		case tc.len != 0 && (rng == nil || rng.Offset != tc.offset || rng.Length != tc.len):
			t.Errorf("%s: range %+v, want %d+%d", tc.header, rng, tc.offset, tc.len)
		}
	}
	if _, ok := parseRange("bytes=0-", 0); ok {
		t.Error("a range of an empty file is unsatisfiable")
	}
}

func TestContent(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	dir := e.mkdir(t, e.root.ID, "Документы")
	f := e.file(t, dir.ID, `отчёт "1".txt`, "0123456789")
	empty := e.file(t, dir.ID, "empty", "")
	path := "/api/v1/nodes/" + f.ID + "/content"
	etag := `"` + f.ETag + `"`

	rec, _ := e.do(t, "GET", path, nil)
	h := rec.Header()
	if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" || h.Get("Content-Length") != "10" || h.Get("ETag") != etag ||
		h.Get("Accept-Ranges") != "bytes" || h.Get("Content-Type") != "text/plain; charset=utf-8" ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "sandbox" || h.Get("Last-Modified") == "" {
		t.Fatalf("full: %d %q %v", rec.Code, rec.Body, h)
	}
	if cd := h.Get("Content-Disposition"); cd != `attachment; filename="_____ _1_.txt"; filename*=UTF-8''%D0%BE%D1%82%D1%87%D1%91%D1%82%20%221%22.txt` {
		t.Fatalf("Content-Disposition %q", cd)
	}

	lastModified := h.Get("Last-Modified")
	future := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	past := time.Now().Add(-24 * time.Hour).UTC().Format(http.TimeFormat)
	for _, tc := range []struct {
		name         string
		header       map[string]string
		code         int
		body, cRange string
	}{
		{"range", map[string]string{"Range": "bytes=2-4"}, 206, "234", "bytes 2-4/10"},
		{"suffix", map[string]string{"Range": "bytes=-3"}, 206, "789", "bytes 7-9/10"},
		{"open end", map[string]string{"Range": "bytes=8-"}, 206, "89", "bytes 8-9/10"},
		{"unsatisfiable", map[string]string{"Range": "bytes=10-"}, 416, "", "bytes */10"},
		{"multi-range", map[string]string{"Range": "bytes=0-1,4-5"}, 200, "0123456789", ""},
		{"malformed", map[string]string{"Range": "bytes=oops"}, 200, "0123456789", ""},
		{"if-none-match", map[string]string{"If-None-Match": etag}, 304, "", ""},
		{"if-none-match weak", map[string]string{"If-None-Match": `"x", W/` + etag}, 304, "", ""},
		{"if-none-match star", map[string]string{"If-None-Match": "*"}, 304, "", ""},
		{"if-none-match stale", map[string]string{"If-None-Match": `"old"`}, 200, "0123456789", ""},
		{"if-range etag", map[string]string{"Range": "bytes=0-1", "If-Range": etag}, 206, "01", "bytes 0-1/10"},
		{"if-range stale etag", map[string]string{"Range": "bytes=0-1", "If-Range": `"old"`}, 200, "0123456789", ""},
		{"if-range date", map[string]string{"Range": "bytes=0-1", "If-Range": lastModified}, 206, "01", "bytes 0-1/10"},
		{"if-range later date", map[string]string{"Range": "bytes=0-1", "If-Range": future}, 206, "01", "bytes 0-1/10"},
		{"if-range old date", map[string]string{"Range": "bytes=0-1", "If-Range": past}, 200, "0123456789", ""},
	} {
		rec, _ := e.do(t, "GET", path, tc.header)
		if rec.Code != tc.code || rec.Body.String() != tc.body || rec.Header().Get("Content-Range") != tc.cRange {
			t.Errorf("%s: %d %q Content-Range %q", tc.name, rec.Code, rec.Body, rec.Header().Get("Content-Range"))
		}
		if tc.code == 304 && rec.Header().Get("ETag") != etag {
			t.Errorf("%s: 304 without ETag", tc.name)
		}
	}

	rec, _ = e.do(t, "HEAD", path, nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "10" {
		t.Fatalf("head: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	rec, _ = e.do(t, "GET", "/api/v1/nodes/"+empty.ID+"/content", nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "0" {
		t.Fatalf("empty file: %d %v", rec.Code, rec.Header())
	}

	var bob string
	if err := e.pool.QueryRow(ctx, `INSERT INTO users (username) VALUES ('bob') RETURNING id`).Scan(&bob); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE nodes SET owner_id = $1 WHERE id = $2`, bob, empty.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		code int
		err  string
	}{
		{"/api/v1/nodes/" + dir.ID + "/content", 400, "not_a_file"},
		{"/api/v1/nodes/" + empty.ID + "/content", 404, "not_found"},
		{"/api/v1/nodes/root/content", 404, "not_found"},
		{"/api/v1/nodes/00000000-0000-0000-0000-000000000000/content", 404, "not_found"},
	} {
		rec, _ := e.do(t, "GET", tc.path, nil)
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), `"code":"`+tc.err+`"`) {
			t.Errorf("%s: %d %s", tc.path, rec.Code, rec.Body)
		}
	}
	if rec, _ := e.get(t, path, false); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d", rec.Code)
	}
}

func readZip(t *testing.T, body []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		if f.Method != zip.Store {
			t.Errorf("%s: method %d, want store", f.Name, f.Method)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		got[f.Name] = string(data)
	}
	return got
}

func TestArchive(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "Документы")
	e.file(t, docs.ID, "a.txt", "alpha")
	sub := e.mkdir(t, docs.ID, "sub")
	e.file(t, sub.ID, "b.bin", strings.Repeat("b", 100_000))
	e.mkdir(t, docs.ID, "Пусто")
	trashed := e.file(t, docs.ID, "trashed.txt", "x")
	if _, err := e.pool.Exec(ctx, `UPDATE nodes SET trashed_at = now(), trash_root = true WHERE id = $1`, trashed.ID); err != nil {
		t.Fatal(err)
	}
	top := e.file(t, e.root.ID, "top.txt", "top")

	rec, aborted := e.do(t, "GET", "/api/v1/nodes/"+docs.ID+"/archive", nil)
	if rec.Code != http.StatusOK || aborted || rec.Header().Get("Content-Type") != "application/zip" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="_________.zip"; filename*=UTF-8''%D0%94%D0%BE%D0%BA%D1%83%D0%BC%D0%B5%D0%BD%D1%82%D1%8B.zip` {
		t.Fatalf("archive: %d %v", rec.Code, rec.Header())
	}
	got := readZip(t, rec.Body.Bytes())
	want := map[string]string{"a.txt": "alpha", "sub/": "", "sub/b.bin": strings.Repeat("b", 100_000), "Пусто/": ""}
	if len(got) != len(want) {
		t.Fatalf("entries %v", slices.Sorted(maps.Keys(got)))
	}
	for name, content := range want {
		if got[name] != content {
			t.Errorf("%s: %d bytes, want %d", name, len(got[name]), len(content))
		}
	}

	rec, _ = e.do(t, "GET", "/api/v1/nodes/root/archive", nil)
	if cd := rec.Header().Get("Content-Disposition"); rec.Code != http.StatusOK || cd != `attachment; filename="files.zip"` {
		t.Fatalf("root archive: %d %q", rec.Code, cd)
	}
	if got := readZip(t, rec.Body.Bytes()); got["top.txt"] != "top" || got["Документы/sub/b.bin"] == "" || len(got) != 6 {
		t.Fatalf("root archive entries: %d", len(got))
	}

	for _, tc := range []struct {
		path string
		code int
		err  string
	}{
		{"/api/v1/nodes/" + top.ID + "/archive", 400, "not_a_directory"},
		{"/api/v1/nodes/bad/archive", 404, "not_found"},
	} {
		rec, _ := e.do(t, "GET", tc.path, nil)
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), `"code":"`+tc.err+`"`) {
			t.Errorf("%s: %d %s", tc.path, rec.Code, rec.Body)
		}
	}
}

// Large entries take a second path; lowering the threshold sends every
// file down it. zip.Reader checks each entry's CRC-32 as it reads.
func TestArchiveLargeEntries(t *testing.T) {
	defer func(old int64) { zip64Size = old }(zip64Size)
	zip64Size = 1
	e := newEnv(t, nil)
	dir := e.mkdir(t, e.root.ID, "d")
	f := e.file(t, dir.ID, "фото.bin", strings.Repeat("ab", 50_000))
	e.file(t, dir.ID, "empty", "")

	rec, aborted := e.do(t, "GET", "/api/v1/nodes/"+dir.ID+"/archive", nil)
	if rec.Code != http.StatusOK || aborted {
		t.Fatalf("archive: %d", rec.Code)
	}
	if got := readZip(t, rec.Body.Bytes()); got["фото.bin"] != strings.Repeat("ab", 50_000) || len(got) != 2 {
		t.Fatalf("entries: %d", len(got))
	}
	zr, _ := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	for _, zf := range zr.File {
		if zf.Name == "фото.bin" && (zf.Flags&0x800 == 0 || zf.Flags&0x8 != 0 || !zf.Modified.Equal(f.MTime.Truncate(time.Second))) {
			t.Errorf("header: flags %#x, modified %s, want %s", zf.Flags, zf.Modified, f.MTime)
		}
	}
}

// brokenReads is storage whose reads fail halfway through.
type brokenReads struct{ storage.Backend }

var errDisk = errors.New("disk on fire")

func (b brokenReads) Get(ctx context.Context, key string, rng *storage.Range) (io.ReadCloser, error) {
	rc, err := b.Backend.Get(ctx, key, rng)
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(io.LimitReader(rc, 3), iotest.ErrReader(errDisk)), rc}, nil
}

func TestStorageFailureCutsResponse(t *testing.T) {
	e := newEnv(t, func(b storage.Backend) storage.Backend { return brokenReads{b} })
	dir := e.mkdir(t, e.root.ID, "d")
	f := e.file(t, dir.ID, "a.txt", "0123456789")

	if rec, aborted := e.do(t, "GET", "/api/v1/nodes/"+f.ID+"/content", nil); !aborted {
		t.Fatalf("content not cut: %d %q", rec.Code, rec.Body)
	}
	rec, aborted := e.do(t, "GET", "/api/v1/nodes/"+dir.ID+"/archive", nil)
	if !aborted {
		t.Fatalf("archive not cut: %d", rec.Code)
	}
	if _, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len())); err == nil {
		t.Fatal("a cut archive still reads as a complete zip")
	}
}
