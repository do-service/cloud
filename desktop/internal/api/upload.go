package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// tus 1.0 (https://tus.io/protocols/resumable-upload), the parts the
// server's tusd uses: creation, PATCH, HEAD to resume. A small client of
// our own rather than a library: it is ~200 lines, and the server's
// extensions (metadata, Upload-Node-* headers, 412) are most of it.

const (
	tusVersion  = "1.0.0"
	uploadsPath = "/api/v1/uploads/"
)

// What to do when the name is taken, for a new file (Upload.OnConflict).
const (
	ConflictFail    = "fail"
	ConflictRename  = "rename"
	ConflictReplace = "replace"
)

type Upload struct {
	// A new file: Name in folder ParentID (RootID for the root), with
	// OnConflict ("" is the server's default, rename).
	ParentID   string
	Name       string
	OnConflict string
	// A new version of file NodeID instead: only if it still has ETag
	// IfMatch (the base the change was made on), else ErrPrecondition,
	// at creation or at the end.
	NodeID  string
	IfMatch string

	Size int64
	// MTime is the local file's modification time; zero lets the server
	// use its own clock.
	MTime time.Time
	// SHA256 (hex), when set, has the server refuse other content.
	SHA256 string
	// Body is read at offsets, so a resume can start mid-file; an
	// *os.File will do. It must stay the same content for the whole
	// upload — the engine uploads a snapshot or checks the file after.
	Body io.ReaderAt

	// URL is an upload begun before (from Progress), to carry on with;
	// "" starts a new one. One that expired or doesn't match is
	// replaced by a new one, reported through Progress.
	URL string
	// Progress, when set, is called with the upload's URL and how much
	// of it the server has, after the creation and after every chunk:
	// persist it (state.transfers) to resume after a restart. An error
	// from it stops the upload.
	Progress func(url string, offset int64) error
}

type UploadResult struct {
	// NodeID is the file the upload became; ETag its new ETag (unquoted):
	// the engine's next base, without another request.
	NodeID string
	ETag   string
}

// Upload uploads u over tus in chunks of Options.ChunkSize, resuming
// u.URL if given. A chunk cut off midway is resumed from what the server
// got (HEAD), with backoff; Options.Retry.Attempts failures in a row
// without progress give up.
//
// On an error other than the network's (412, 413/507, 409 name
// conflict, …) the server has dropped the upload: forget its URL.
//
// If the response that completes an upload is lost, the file exists but
// the client can't tell: resuming then finds the upload gone and starts
// a new one, which for NodeID+IfMatch ends in ErrPrecondition (the file
// has the new ETag already) and for a new file in a renamed duplicate
// or a conflict. The engine compares the node's SHA-256 with what it
// uploaded to tell.
func (c *Client) Upload(ctx context.Context, u Upload) (UploadResult, error) {
	if u.Size < 0 || u.Body == nil {
		return UploadResult{}, errors.New("api: upload needs a body and its size")
	}
	t := &tusUpload{c: c, u: u, url: u.URL, offset: -1}
	failures := 0
	for {
		res, done, err := t.step(ctx)
		if done {
			return res, err
		}
		if err == nil {
			failures = 0
			continue
		}
		var p permanent
		if errors.As(err, &p) {
			return UploadResult{}, p.err
		}
		failures++
		if ctx.Err() != nil {
			return UploadResult{}, ctx.Err()
		}
		if failures >= c.retry.Attempts || !retryable(err, true) {
			return UploadResult{}, err
		}
		// Where the server is now isn't known after a failed PATCH.
		t.offset = -1
		if err := c.sleep(ctx, c.delay(failures, err)); err != nil {
			return UploadResult{}, err
		}
	}
}

type tusUpload struct {
	c   *Client
	u   Upload
	url string
	// offset is what the server has, -1 when it must be asked (HEAD).
	offset int64
}

// step does one request: HEAD to learn the offset, the creation, or a
// chunk. done means the upload is over, with res or err.
func (t *tusUpload) step(ctx context.Context) (res UploadResult, done bool, err error) {
	switch {
	case t.url == "":
		return t.create(ctx)
	case t.offset < 0:
		return UploadResult{}, false, t.head(ctx)
	default:
		return t.patch(ctx)
	}
}

func (t *tusUpload) progress() error {
	if t.u.Progress == nil {
		return nil
	}
	if err := t.u.Progress(t.url, t.offset); err != nil {
		return permanent{err}
	}
	return nil
}

func (t *tusUpload) newRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := t.c.newRequest(ctx, method, url, body, true)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Tus-Resumable", tusVersion)
	return req, nil
}

func (t *tusUpload) create(ctx context.Context) (UploadResult, bool, error) {
	var res *http.Response
	// A POST is repeated only when it certainly didn't reach the server:
	// otherwise a second upload would reserve the quota twice.
	err := t.c.withRetry(ctx, false, func(ctx context.Context) error {
		req, err := t.newRequest(ctx, http.MethodPost, t.c.endpoint(uploadsPath, nil), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Upload-Length", strconv.FormatInt(t.u.Size, 10))
		req.Header.Set("Upload-Metadata", t.metadata())
		if res, err = t.c.hc.Do(req); err != nil {
			return err
		}
		if res.StatusCode != http.StatusCreated {
			return readError(res)
		}
		res.Body.Close()
		return nil
	})
	if err != nil {
		return UploadResult{}, true, err
	}
	loc, err := t.c.resolve(res.Header.Get("Location"))
	if err != nil || res.Header.Get("Location") == "" {
		return UploadResult{}, true, fmt.Errorf("api: upload created without a usable Location: %v", err)
	}
	t.url, t.offset = loc, 0
	// An empty file is complete as created: the answer is the final one.
	if t.u.Size == 0 {
		return finished(res)
	}
	return UploadResult{}, false, t.progress()
}

func (t *tusUpload) metadata() string {
	meta := map[string]string{"mtime": ""}
	if t.u.NodeID != "" {
		meta["nodeId"], meta["ifMatch"] = t.u.NodeID, t.u.IfMatch
	} else {
		meta["filename"], meta["parentId"], meta["onConflict"] = t.u.Name, t.u.ParentID, t.u.OnConflict
	}
	if !t.u.MTime.IsZero() {
		meta["mtime"] = t.u.MTime.UTC().Format(time.RFC3339Nano)
	}
	meta["sha256"] = t.u.SHA256
	keys := make([]string, 0, len(meta))
	for k, v := range meta {
		if v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + " " + base64.StdEncoding.EncodeToString([]byte(meta[k]))
	}
	return strings.Join(parts, ",")
}

// head asks the server how much of the upload it has. A gone upload
// (expired and cleaned up, finished, cancelled) or one of another size
// is started over.
func (t *tusUpload) head(ctx context.Context) error {
	req, err := t.newRequest(ctx, http.MethodHead, t.url, nil)
	if err != nil {
		return err
	}
	res, err := t.c.hc.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone:
		t.url = ""
		return nil
	case res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNoContent:
		return &APIError{Status: res.StatusCode, RetryAfter: retryAfter(res.Header.Get("Retry-After"), time.Now())}
	}
	offset, err1 := strconv.ParseInt(res.Header.Get("Upload-Offset"), 10, 64)
	length, err2 := strconv.ParseInt(res.Header.Get("Upload-Length"), 10, 64)
	if err1 != nil || err2 != nil || length != t.u.Size || offset < 0 || offset >= length {
		// Not this file's upload (it changed size since), or one complete
		// but not turned into a file: a new one. The old one's
		// reservation goes with the server's cleanup.
		t.url = ""
		return nil
	}
	t.offset = offset
	return t.progress()
}

func (t *tusUpload) patch(ctx context.Context) (UploadResult, bool, error) {
	n := min(t.c.chunk, t.u.Size-t.offset)
	src := &sourceReader{r: io.NewSectionReader(t.u.Body, t.offset, n)}
	var body io.Reader = src
	if t.c.upLim != nil {
		body = limitReader(ctx, body, t.c.upLim)
	}
	req, err := t.newRequest(ctx, http.MethodPatch, t.url, body)
	if err != nil {
		return UploadResult{}, false, err
	}
	req.ContentLength = n
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set("Upload-Offset", strconv.FormatInt(t.offset, 10))
	res, err := t.c.hc.Do(req)
	if err != nil {
		if src.err != nil {
			// The local file, not the network: it shrank or can't be
			// read, and a retry would fail alike.
			return UploadResult{}, true, fmt.Errorf("api: upload: reading the file: %w", src.err)
		}
		return UploadResult{}, false, err
	}
	switch res.StatusCode {
	case http.StatusNoContent, http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		// Expired between two chunks: start over.
		res.Body.Close()
		t.url = ""
		return UploadResult{}, false, nil
	case http.StatusConflict:
		// tusd's offset mismatch (a PATCH of ours it saw but we didn't),
		// or the API's name conflict at the end.
		err := readError(res)
		var ae *APIError
		if errors.As(err, &ae) && ae.Code != "" && ae.Code != "ERR_MISMATCHED_OFFSET" {
			return UploadResult{}, true, err
		}
		t.offset = -1
		return UploadResult{}, false, nil
	default:
		err := readError(res)
		var ae *APIError
		if errors.As(err, &ae) && (ae.Status >= 500 || ae.Status == http.StatusTooManyRequests) {
			return UploadResult{}, false, err
		}
		return UploadResult{}, true, err
	}
	res.Body.Close()
	offset, err := strconv.ParseInt(res.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset <= t.offset || offset > t.u.Size {
		return UploadResult{}, true, fmt.Errorf("api: upload: bad Upload-Offset %q after %d", res.Header.Get("Upload-Offset"), t.offset)
	}
	t.offset = offset
	if offset == t.u.Size {
		return finished(res)
	}
	return UploadResult{}, false, t.progress()
}

// sourceReader remembers its reader's failure, to tell it from the
// network's. A short read (the file shrank) is one too: the request
// declared the chunk's length.
type sourceReader struct {
	r   *io.SectionReader
	n   int64
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.n += int64(n)
	if err == io.EOF && s.n < s.r.Size() {
		err = io.ErrUnexpectedEOF
	}
	if err != nil && err != io.EOF {
		s.err = err
	}
	return n, err
}

// finished reads the result off the response that completed the upload.
func finished(res *http.Response) (UploadResult, bool, error) {
	r := UploadResult{NodeID: res.Header.Get("Upload-Node-Id"), ETag: unquoteETag(res.Header.Get("Upload-Node-Etag"))}
	if r.NodeID == "" {
		return UploadResult{}, true, errors.New("api: upload complete but the server named no file (Upload-Node-Id)")
	}
	return r, true, nil
}
