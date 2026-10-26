package nodes

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/storage"
)

// Outcomes of the conditional and HEAD checks that need no content: pick
// returns them to stop fs.OpenRange before it opens anything.
var (
	errNotModified = errors.New("not modified")
	errNoBody      = errors.New("no body")
	errBadRange    = errors.New("range not satisfiable")
)

// content downloads a file; with ?inline=1 one the browser can show
// safely (an image, a PDF) is shown instead: the viewer's way in.
func (h *Handlers) content(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	h.serve(w, r, r.URL.Query().Get("inline") == "1", func(pick func(fs.Node) (*storage.Range, error)) (io.ReadCloser, fs.Node, error) {
		return h.FS.OpenRange(r.Context(), u.ID, r.PathValue("id"), pick)
	})
}

// versionContent downloads an earlier version of a file.
func (h *Handlers) versionContent(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	h.serve(w, r, false, func(pick func(fs.Node) (*storage.Range, error)) (io.ReadCloser, fs.Node, error) {
		return h.FS.OpenVersion(r.Context(), u.ID, r.PathValue("id"), pick)
	})
}

// Opener opens content, choosing the range with pick from the node it
// opens (fs.OpenRange, fs.OpenVersion).
type Opener func(pick func(fs.Node) (*storage.Range, error)) (io.ReadCloser, fs.Node, error)

func (h *Handlers) serve(w http.ResponseWriter, r *http.Request, inline bool, open Opener) {
	if err := Serve(w, r, inline, open); err != nil {
		h.fail(w, r, err)
	}
}

// Serve answers a download from open — for the API, public links and
// WebDAV alike. One byte range per request is served (206); a multi-range
// request gets the whole file, which RFC 9110 allows. An error from open
// is returned with nothing written, for the caller to answer in its
// protocol's terms.
func Serve(w http.ResponseWriter, r *http.Request, inline bool, open Opener) error {
	var node fs.Node
	var rng *storage.Range
	pick := func(n fs.Node) (*storage.Range, error) {
		node = n
		if etagMatches(r.Header.Get("If-None-Match"), quoteETag(n.ETag)) {
			return nil, errNotModified
		}
		if hdr := r.Header.Get("Range"); hdr != "" && ifRangeHolds(r.Header.Get("If-Range"), n) {
			var ok bool
			if rng, ok = parseRange(hdr, n.Size); !ok {
				// Only an unsatisfiable range gets here: malformed ones
				// are ignored, as RFC 9110 asks.
				return nil, errBadRange
			}
		}
		if r.Method == http.MethodHead {
			return nil, errNoBody
		}
		return rng, nil
	}
	rc, _, err := open(pick)
	switch {
	case errors.Is(err, errNotModified):
		setValidators(w, node)
		w.WriteHeader(http.StatusNotModified)
		return nil
	case errors.Is(err, errBadRange):
		setValidators(w, node)
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(node.Size, 10))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return nil
	case errors.Is(err, errNoBody):
	case err != nil:
		return err
	default:
		defer rc.Close()
	}

	hdr := w.Header()
	setValidators(w, node)
	contentType := node.MIME
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	hdr.Set("Content-Type", contentType)
	hdr.Set("X-Content-Type-Options", "nosniff")
	if inline && viewable(contentType) {
		// Shown in the browser, for a view-only link: only types that
		// can't carry script, declared as such (nosniff).
		// With its name, which the browser's PDF viewer shows and saves as.
		hdr.Set("Content-Disposition", "inline"+strings.TrimPrefix(attachment(node.Name), "attachment"))
	} else {
		// A download: served inline from the app's origin, a user's HTML
		// or SVG would run as the app (stored XSS). sandbox is a second
		// line for clients that render anyway.
		hdr.Set("Content-Disposition", attachment(node.Name))
		hdr.Set("Content-Security-Policy", "sandbox")
	}
	hdr.Set("Accept-Ranges", "bytes")
	status, length := http.StatusOK, node.Size
	if rng != nil {
		status, length = http.StatusPartialContent, rng.Length
		hdr.Set("Content-Range", "bytes "+strconv.FormatInt(rng.Offset, 10)+"-"+strconv.FormatInt(rng.Offset+rng.Length-1, 10)+"/"+strconv.FormatInt(node.Size, 10))
	}
	hdr.Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	if rc == nil {
		return nil
	}
	if _, err := io.Copy(w, rc); err != nil {
		// Mostly the client going away. Either way the headers are out,
		// so all that's left is cutting the response short.
		slog.DebugContext(r.Context(), "nodes: download interrupted", "node", node.ID, "err", err)
		panic(http.ErrAbortHandler)
	}
	return nil
}

func quoteETag(etag string) string { return `"` + etag + `"` }

// setValidators sets the headers caches revalidate with. private: the
// content is behind a session; no-cache: revalidate before reuse, which a
// matching ETag makes a cheap 304.
func setValidators(w http.ResponseWriter, n fs.Node) {
	hdr := w.Header()
	hdr.Set("ETag", quoteETag(n.ETag))
	hdr.Set("Last-Modified", n.MTime.UTC().Format(http.TimeFormat))
	hdr.Set("Cache-Control", "private, no-cache")
}

// etagMatches implements If-None-Match's weak comparison against etag.
func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	for tag := range strings.SplitSeq(header, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || strings.TrimPrefix(tag, "W/") == etag {
			return true
		}
	}
	return false
}

// ifRangeHolds reports whether a Range request may be served partially:
// If-Range absent, or naming the current content — by a strong ETag, or a
// date no older than the content's (HTTP dates have whole seconds).
func ifRangeHolds(header string, n fs.Node) bool {
	switch {
	case header == "":
		return true
	case strings.HasPrefix(header, `"`):
		return header == quoteETag(n.ETag)
	}
	t, err := http.ParseTime(header)
	return err == nil && !n.MTime.Truncate(time.Second).After(t)
}

// parseRange reads a single "bytes=" range against size. ok is false only
// when the range is valid but unsatisfiable; anything else that isn't one
// satisfiable range (malformed, several ranges, another unit) returns a
// nil range with ok true, meaning "serve it all".
func parseRange(header string, size int64) (rng *storage.Range, ok bool) {
	spec, found := strings.CutPrefix(header, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return nil, true
	}
	first, last, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return nil, true
	}
	if first == "" {
		// Suffix: the last n bytes.
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			return nil, true
		}
		if n == 0 || size == 0 {
			return nil, false
		}
		n = min(n, size)
		return &storage.Range{Offset: size - n, Length: n}, true
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return nil, true
	}
	end := size - 1
	if last != "" {
		if end, err = strconv.ParseInt(last, 10, 64); err != nil || end < start {
			return nil, true
		}
		end = min(end, size-1)
	}
	if start >= size {
		return nil, false
	}
	return &storage.Range{Offset: start, Length: end - start + 1}, true
}

// attachment is a Content-Disposition for downloading under name, as RFC
// 6266 suggests: filename* carries the real name percent-encoded (RFC
// 8187) for browsers, filename an ASCII stand-in for clients that only
// read that one (curl -J, wget).
func attachment(name string) string {
	var ascii, encoded strings.Builder
	for _, b := range []byte(name) {
		if b < 0x80 && b != '"' && b != '\\' {
			ascii.WriteByte(b)
		} else if utf8.RuneStart(b) {
			// One stand-in per character: per byte, "ё" would be "__".
			ascii.WriteByte('_')
		}
		if isAttrChar(b) {
			encoded.WriteByte(b)
		} else {
			fmt.Fprintf(&encoded, "%%%02X", b)
		}
	}
	if ascii.String() == name {
		return `attachment; filename="` + name + `"`
	}
	return `attachment; filename="` + ascii.String() + `"; filename*=UTF-8''` + encoded.String()
}

// isAttrChar is RFC 8187's attr-char: what filename* carries unencoded.
func isAttrChar(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' || strings.IndexByte("!#$&+-.^_`|~", b) >= 0
}

// viewable reports whether a type is safe to show inline from the app's
// origin: media and plain text never run script; HTML, SVG and the like
// would.
func viewable(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.TrimSpace(strings.ToLower(mediaType))
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/bmp",
		"application/pdf", "text/plain":
		return true
	}
	return strings.HasPrefix(mediaType, "video/") || strings.HasPrefix(mediaType, "audio/")
}
