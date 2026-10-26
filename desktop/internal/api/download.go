package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// ErrChecksum: the downloaded content doesn't match the SHA-256 the
// caller expected. The partial file is truncated, so a retry starts over.
var ErrChecksum = errors.New("api: downloaded content doesn't match its sha256")

type DownloadOptions struct {
	// ETag is the content the caller wants (the node's ETag). Bytes
	// already in the file are kept only while the server still has that
	// content (If-Range); "" keeps none.
	ETag string
	// SHA256 (hex), when set, is checked against the whole file, unless
	// the content changed on the server (DownloadResult.ETag differs).
	SHA256 string
}

type DownloadResult struct {
	// ETag of the content downloaded (unquoted): the new base.
	ETag string
	// Size of the whole file.
	Size int64
	// SHA256 (hex) of the whole file, computed while downloading.
	SHA256 string
	// Resumed: bytes the file had from before were kept.
	Resumed bool
}

// DownloadFile downloads file id into path, normally the engine's
// ".~cloud-<id>.part": if path already has bytes of the same content
// (opts.ETag), only the rest is requested (Range + If-Range), so a
// download carries on after an interruption or a restart. A failure
// midway is retried the same way. Flushing the file and renaming it into
// place are the caller's (localfs.Commit): an fsync here would be a
// second one, and on macOS a full one costs milliseconds a file.
func (c *Client) DownloadFile(ctx context.Context, id, path string, opts DownloadOptions) (DownloadResult, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return DownloadResult{}, err
	}
	defer f.Close()
	d := &download{c: c, id: id, f: f, etag: opts.ETag, h: sha256.New()}
	if opts.ETag == "" {
		if err := d.truncate(); err != nil {
			return DownloadResult{}, err
		}
	} else if d.offset, err = f.Seek(0, io.SeekEnd); err != nil {
		return DownloadResult{}, err
	}
	d.resumed = d.offset > 0
	if err := c.withRetry(ctx, true, d.attempt); err != nil {
		return DownloadResult{}, err
	}
	if err := d.rehash(); err != nil {
		return DownloadResult{}, err
	}
	sum := hex.EncodeToString(d.h.Sum(nil))
	// Only for the content asked for: one changed meanwhile came whole,
	// with another ETag, and the caller's SHA-256 is of the old one.
	if opts.SHA256 != "" && (opts.ETag == "" || d.etag == opts.ETag) && !strings.EqualFold(sum, opts.SHA256) {
		_ = d.truncate()
		return DownloadResult{}, fmt.Errorf("%w: got %s, want %s", ErrChecksum, sum, opts.SHA256)
	}
	return DownloadResult{ETag: d.etag, Size: d.size, SHA256: sum, Resumed: d.resumed}, nil
}

type download struct {
	c    *Client
	id   string
	f    *os.File
	etag string
	// offset is how much of the file is there; hashed how much of it
	// went into h, which lags only after a resume from disk.
	offset, hashed int64
	h              hash.Hash
	size           int64
	resumed        bool
	done           bool
}

func (d *download) truncate() error {
	d.offset, d.hashed, d.resumed = 0, 0, false
	d.h.Reset()
	if err := d.f.Truncate(0); err != nil {
		return err
	}
	_, err := d.f.Seek(0, io.SeekStart)
	return err
}

// rehash feeds h the bytes of the file it hasn't seen: the part kept
// from before this call.
func (d *download) rehash() error {
	if d.hashed == d.offset {
		return nil
	}
	if _, err := io.Copy(d.h, io.NewSectionReader(d.f, d.hashed, d.offset-d.hashed)); err != nil {
		return err
	}
	d.hashed = d.offset
	return nil
}

func (d *download) attempt(ctx context.Context) error {
	if d.done {
		return nil
	}
	req, err := d.c.newRequest(ctx, http.MethodGet, d.c.endpoint(nodePath(d.id)+"/content", nil), nil, true)
	if err != nil {
		return err
	}
	if d.offset > 0 && d.etag != "" {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(d.offset, 10)+"-")
		req.Header.Set("If-Range", quoteETag(d.etag))
	}
	res, err := d.c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		// The whole content: no range asked, or the content changed.
		if d.offset > 0 {
			if err := d.truncate(); err != nil {
				return permanent{err}
			}
		}
		if d.size = res.ContentLength; d.size < 0 {
			return fmt.Errorf("api: download without Content-Length")
		}
	case http.StatusPartialContent:
		start, size, ok := contentRange(res.Header.Get("Content-Range"))
		if !ok || start != d.offset {
			return permanent{fmt.Errorf("api: download: unexpected Content-Range %q from offset %d", res.Header.Get("Content-Range"), d.offset)}
		}
		d.size = size
	case http.StatusRequestedRangeNotSatisfiable:
		// If-Range held, so the content is the one we have: complete if
		// it has all the bytes, else the file is longer than the content
		// and something else wrote to it.
		_, size, _ := contentRange(res.Header.Get("Content-Range"))
		if size == d.offset {
			d.size, d.done = size, true
			return nil
		}
		if err := d.truncate(); err != nil {
			return permanent{err}
		}
		return fmt.Errorf("api: download: partial file longer than the content, starting over: %w", io.ErrUnexpectedEOF)
	default:
		return readError(res)
	}
	if et := res.Header.Get("ETag"); et != "" {
		d.etag = unquoteETag(et)
	}
	if err := d.rehash(); err != nil {
		return permanent{err}
	}
	want := d.size - d.offset
	var body io.Reader = res.Body
	if d.c.downLim != nil {
		body = limitReader(ctx, body, d.c.downLim)
	}
	// A short body is retried from where it stopped; the bytes that did
	// come stay in the file and the hash.
	n, err := io.Copy(io.MultiWriter(d.f, d.h), body)
	d.offset += n
	d.hashed += n
	if err == nil && n != want {
		err = fmt.Errorf("api: download: got %d of %d bytes: %w", n, want, io.ErrUnexpectedEOF)
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		// The local disk, not the network: no use repeating.
		return permanent{err}
	}
	if err == nil {
		d.done = true
	}
	return err
}

// contentRange parses "bytes first-last/size" or "bytes */size".
func contentRange(v string) (start, size int64, ok bool) {
	rest, found := strings.CutPrefix(v, "bytes ")
	if !found {
		return 0, 0, false
	}
	span, total, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	size, err := strconv.ParseInt(total, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if span == "*" {
		return 0, size, true
	}
	first, _, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, false
	}
	start, err = strconv.ParseInt(first, 10, 64)
	return start, size, err == nil
}
