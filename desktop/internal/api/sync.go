package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIVersion is the version of the sync API this client speaks
// (/sync/info apiVersion).
const APIVersion = 1

var (
	// ErrIncompatible: the server speaks another sync API version.
	ErrIncompatible = errors.New("api: the server's sync API version isn't supported by this client")
	// ErrClientTooOld: the server asks for a newer client (minClientVersion).
	ErrClientTooOld = errors.New("api: this client is older than the server supports; update it")
	// ErrTruncated: a tree stream ended before it was complete. The
	// server cuts the response off on a failure midway, so a partial
	// tree can't pass for a whole one.
	ErrTruncated = errors.New("api: the tree stream was cut off")
)

// Info is GET /api/v1/sync/info.
type Info struct {
	APIVersion       int
	ServerVersion    string
	MinClientVersion string
	// JournalRetention is how long a client may stay offline before its
	// cursor expires.
	JournalRetention time.Duration
	// MaxWait is the longest long poll of Changes the server holds.
	MaxWait time.Duration
}

// Info asks the server about itself, without signing in: a client
// checks an address before it opens the browser to sign in there. The
// error is ErrIncompatible or ErrClientTooOld (with Info filled in, for
// the message) when this client can't sync with it.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var raw struct {
		APIVersion       int    `json:"apiVersion"`
		ServerVersion    string `json:"serverVersion"`
		MinClientVersion string `json:"minClientVersion"`
		JournalRetention int64  `json:"journalRetention"`
		MaxWait          int    `json:"maxWait"`
	}
	if err := c.do(ctx, call{method: http.MethodGet, path: "/api/v1/sync/info", out: &raw, noAuth: true}); err != nil {
		return Info{}, err
	}
	info := Info{
		APIVersion: raw.APIVersion, ServerVersion: raw.ServerVersion, MinClientVersion: raw.MinClientVersion,
		JournalRetention: time.Duration(raw.JournalRetention) * time.Second, MaxWait: time.Duration(raw.MaxWait) * time.Second,
	}
	if info.APIVersion != APIVersion {
		return info, fmt.Errorf("%w: server %d, client %d", ErrIncompatible, info.APIVersion, APIVersion)
	}
	if olderVersion(c.version, info.MinClientVersion) {
		return info, fmt.Errorf("%w: %s, at least %s needed", ErrClientTooOld, c.version, info.MinClientVersion)
	}
	return info, nil
}

// Ping asks /sync/info once, without retries and within 10 s: whether
// the server answers at all, for a client that is offline to notice
// quickly when it is back: nil when it answers 2xx.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodGet, c.endpoint("/api/v1/sync/info", nil), nil, false)
	if err != nil {
		return err
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return readError(res)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxErrorBody))
	return res.Body.Close()
}

// olderVersion reports whether semver v is older than min, comparing
// major.minor.patch only: a pre-release build counts as its release, so
// a development build isn't locked out by its own version.
func olderVersion(v, min string) bool {
	a, b := versionCore(v), versionCore(min)
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func versionCore(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(part)
	}
	return out
}

// Node is a file or folder as the sync and nodes APIs report it.
type Node struct {
	ID string `json:"id"`
	// ParentID is "" at the top of what the user sees: their root, or a
	// node shared with them whose folder isn't.
	ParentID string `json:"-"`
	Name     string `json:"name"`
	// Kind is KindFile or KindDir.
	Kind string `json:"kind"`
	Size int64  `json:"size"`
	// ETag is unquoted; it is the base of conditional writes.
	ETag string `json:"etag"`
	// SHA256 is hex; "" for folders and files without a known hash.
	SHA256 string    `json:"sha256,omitempty"`
	MIME   string    `json:"mime,omitempty"`
	MTime  time.Time `json:"mtime"`
	// Permissions: "read", "write", "reshare".
	Permissions []string `json:"permissions"`
}

const (
	KindFile = "file"
	KindDir  = "dir"
)

func (n *Node) UnmarshalJSON(b []byte) error {
	type plain Node
	var raw struct {
		plain
		ParentID *string `json:"parentId"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*n = Node(raw.plain)
	if raw.ParentID != nil {
		n.ParentID = *raw.ParentID
	}
	return nil
}

// Can reports whether the user has permission p ("write", …) on n.
func (n Node) Can(p string) bool {
	for _, have := range n.Permissions {
		if have == p {
			return true
		}
	}
	return false
}

type Owner struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Root is where a sync starts: the user's own root or something shared
// with them.
type Root struct {
	ID string `json:"id"`
	// Kind is "own" or "shared".
	Kind string `json:"kind"`
	// Name is "" for the own root; «Имя (Владелец)» for a shared one.
	Name        string   `json:"name"`
	Owner       Owner    `json:"owner"`
	Permissions []string `json:"permissions"`
	ETag        string   `json:"etag"`
}

type Roots struct {
	Roots []Root `json:"roots"`
	// Cursor is from before the listing: changes from it cover the trees
	// read after.
	Cursor string `json:"cursor"`
}

// Roots lists the user's sync roots.
func (c *Client) Roots(ctx context.Context) (Roots, error) {
	var r Roots
	err := c.do(ctx, call{method: http.MethodGet, path: "/api/v1/sync/roots", out: &r})
	return r, err
}

// maxTreeLine bounds one NDJSON line: a node is a few hundred bytes.
const maxTreeLine = 1 << 20

// Tree streams the subtree of root (a node id, or "root" for the user's
// own) to fn, one node at a time, the root first and every folder before
// its contents, all of one snapshot; it returns the cursor of that
// snapshot. fn's error stops the stream and is returned as is.
//
// A stream that ends early is ErrTruncated: never take what fn got then
// for the whole tree. Before the first node a failure is retried like
// any GET; after it, starting over is the caller's call.
func (c *Client) Tree(ctx context.Context, root string, fn func(Node) error) (string, error) {
	var cursor string
	started := false
	err := c.withRetry(ctx, true, func(ctx context.Context) error {
		req, err := c.newRequest(ctx, http.MethodGet, c.endpoint("/api/v1/sync/tree", url.Values{"root": {root}}), nil, true)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/x-ndjson")
		res, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		if res.StatusCode != http.StatusOK {
			return readError(res)
		}
		defer res.Body.Close()
		cursor, err = readTree(res.Body, func(n Node) error {
			started = true
			if err := fn(n); err != nil {
				return permanent{err}
			}
			return nil
		})
		if err != nil && started && !errors.As(err, new(permanent)) {
			// fn has nodes of this stream: a second one would repeat them.
			if !errors.Is(err, ErrTruncated) && isNetwork(err) {
				err = fmt.Errorf("%w: %w", ErrTruncated, err)
			}
			return permanent{err}
		}
		return err
	})
	var p permanent
	if errors.As(err, &p) {
		err = p.err
	}
	return cursor, err
}

func readTree(body io.Reader, fn func(Node) error) (string, error) {
	br := bufio.NewReaderSize(body, 64<<10)
	var cursor string
	nodes := 0
	for {
		line, err := readLine(br)
		if err == io.EOF {
			// A tree has at least its root: without it the server never
			// finished.
			if cursor == "" || nodes == 0 {
				return "", ErrTruncated
			}
			return cursor, nil
		}
		if err != nil {
			return "", err
		}
		if cursor == "" {
			var first struct {
				Cursor string `json:"cursor"`
			}
			if err := json.Unmarshal(line, &first); err != nil || first.Cursor == "" {
				return "", fmt.Errorf("api: tree: first line isn't the cursor")
			}
			cursor = first.Cursor
			continue
		}
		var n Node
		if err := json.Unmarshal(line, &n); err != nil {
			return "", fmt.Errorf("api: tree: line %d: %w", nodes+2, err)
		}
		nodes++
		if err := fn(n); err != nil {
			return "", err
		}
	}
}

// readLine reads one '\n'-terminated line without it. A last line with
// no '\n' is a stream cut off midway (the server's encoder ends every
// line), not data.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxTreeLine {
			return nil, fmt.Errorf("api: tree: line over %d bytes", maxTreeLine)
		}
		switch {
		case err == nil:
			return bytes.TrimRight(buf, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err == io.EOF && len(buf) == 0:
			return nil, io.EOF
		case err == io.EOF:
			return nil, ErrTruncated
		default:
			// A chunked body the server aborted ends in io.ErrUnexpectedEOF.
			return nil, fmt.Errorf("%w: %w", ErrTruncated, err)
		}
	}
}

// Change is one node changed since the cursor: its current state, or a
// tombstone.
type Change struct {
	ID string `json:"id"`
	// Owner is the id of the tree's owner.
	Owner string `json:"owner"`
	// Deleted: trashed, purged or no longer visible to the user —
	// possibly a node the client never knew.
	Deleted bool  `json:"deleted"`
	Node    *Node `json:"node,omitempty"`
	// Subtree: a folder whose contents the client may lack; read them
	// with Tree(ID).
	Subtree bool `json:"subtree,omitempty"`
}

type Changes struct {
	Changes []Change `json:"changes"`
	Cursor  string   `json:"cursor"`
	// More: there is more to read at once, from Cursor.
	More bool `json:"more"`
	// RootsChanged: list the roots again; new ones need their trees.
	RootsChanged bool `json:"rootsChanged"`
}

// Changes reads what changed since cursor: live nodes in an order where
// a folder comes before what's in it, then tombstones. limit (0: the
// server's 1000) bounds the journal rows read; wait > 0 long-polls up to
// that long (the server caps it at Info.MaxWait) when nothing changed.
// An expired cursor is ErrCursorExpired: resync in full.
func (c *Client) Changes(ctx context.Context, cursor string, limit int, wait time.Duration) (Changes, error) {
	q := url.Values{"cursor": {cursor}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if wait > 0 {
		q.Set("wait", strconv.Itoa(int(wait/time.Second)))
	}
	var ch Changes
	err := c.do(ctx, call{method: http.MethodGet, path: "/api/v1/sync/changes", query: q, out: &ch, extra: wait})
	return ch, err
}
