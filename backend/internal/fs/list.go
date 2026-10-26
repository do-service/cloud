package fs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/do-service/cloud/backend/internal/access"
)

var ErrInvalidCursor = errors.New("fs: invalid cursor")

type Sort string

const (
	SortName  Sort = "name"
	SortMTime Sort = "mtime"
	SortSize  Sort = "size"
)

// Valid reports whether s is one of the sort orders List knows.
func (s Sort) Valid() bool { return s == SortName || s == SortMTime || s == SortSize }

const (
	DefaultListLimit = 200
	MaxListLimit     = 1000
)

type ListOptions struct {
	Sort  Sort // SortName when empty
	Desc  bool
	Limit int // DefaultListLimit when 0, capped at MaxListLimit
	// Cursor is a previous page's next cursor, "" for the first page.
	Cursor string
}

// cursor is the sort key of the last node a page returned; the next page
// starts right after it. Sort and Desc are kept to refuse a cursor reused
// with another order, which would silently skip or repeat nodes.
type cursor struct {
	Sort  Sort   `json:"s"`
	Desc  bool   `json:"d,omitempty"`
	Kind  Kind   `json:"k"`
	Value string `json:"v"`
	ID    string `json:"i"`
}

func encodeCursor(c cursor) string {
	// Marshalling these field types can't fail.
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string, opts ListOptions) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	var c cursor
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return cursor{}, ErrInvalidCursor
	}
	if c.Sort != opts.Sort || c.Desc != opts.Desc || (c.Kind != KindDir && c.Kind != KindFile) || !validID(c.ID) {
		return cursor{}, ErrInvalidCursor
	}
	if _, err := sortValue(c.Sort, c.Value); err != nil {
		return cursor{}, ErrInvalidCursor
	}
	return c, nil
}

// cursorValue renders n's sort key the way sortValue reads it back.
func cursorValue(s Sort, n Node, key string) string {
	switch s {
	case SortMTime:
		return n.MTime.UTC().Format(time.RFC3339Nano)
	case SortSize:
		return strconv.FormatInt(n.Size, 10)
	}
	return key
}

// sortValue is a cursor's sort key as the query parameter for s.
func sortValue(s Sort, v string) (any, error) {
	switch s {
	case SortMTime:
		return time.Parse(time.RFC3339Nano, v)
	case SortSize:
		return strconv.ParseInt(v, 10, 64)
	}
	return v, nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validID screens ids from requests: Postgres rejects a malformed uuid
// with an error, and to the caller it is simply a node that isn't there.
func validID(id string) bool { return uuidPattern.MatchString(id) }

// Root returns the user's root folder.
func (s *Service) Root(ctx context.Context, userID string) (Node, error) {
	return getRoot(ctx, s.db, userID)
}

// Lookup finds the node at names below fromID, one name per level, in one
// statement. Names match as the tree keeps them unique: without regard to
// case. A name that can't exist is simply not found.
func (s *Service) Lookup(ctx context.Context, actorID, fromID string, names []string) (Node, error) {
	if !validID(fromID) {
		return Node{}, ErrNotFound
	}
	keys := make([]string, len(names))
	for i, name := range names {
		_, key, err := NormalizeName(name)
		if err != nil {
			return Node{}, ErrNotFound
		}
		keys[i] = key
	}
	n, err := scanNode(s.db.QueryRow(ctx, `
		WITH RECURSIVE p AS (
			SELECT id, 0 AS depth FROM nodes WHERE id = $1 AND trashed_at IS NULL
			UNION ALL
			SELECT n.id, p.depth + 1 FROM p JOIN nodes n ON n.parent_id = p.id
			WHERE p.depth < cardinality($2::text[]) AND n.name_key = ($2::text[])[p.depth + 1] AND n.trashed_at IS NULL
		)
		SELECT `+nodeColumns+` FROM nodes WHERE id = (SELECT id FROM p WHERE depth = cardinality($2::text[]))`, fromID, keys))
	if err != nil {
		return Node{}, err
	}
	return n, allow(ctx, s.db, actorID, &n, access.Read)
}

// Quota returns the user's used bytes and quota, nil when unlimited.
func (s *Service) Quota(ctx context.Context, userID string) (used int64, quota *int64, err error) {
	err = s.db.QueryRow(ctx, `SELECT used_bytes, quota_bytes FROM users WHERE id = $1`, userID).Scan(&used, &quota)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, ErrNotFound
	}
	return used, quota, err
}

// List returns a page of dirID's children: folders first, then files,
// each group in the requested order, ties broken by id. next is the
// cursor of the following page, "" after the last one. Pages stay
// consistent under concurrent changes in the keyset sense: a node that
// exists throughout is returned exactly once.
func (s *Service) List(ctx context.Context, actorID, dirID string, opts ListOptions) (items []Node, next string, err error) {
	if opts.Sort == "" {
		opts.Sort = SortName
	}
	if !opts.Sort.Valid() {
		// Callers check user input with Valid; this is a bug upstream.
		return nil, "", fmt.Errorf("fs: unknown sort %q", opts.Sort)
	}
	switch {
	case opts.Limit <= 0:
		opts.Limit = DefaultListLimit
	case opts.Limit > MaxListLimit:
		opts.Limit = MaxListLimit
	}
	var after *cursor
	if opts.Cursor != "" {
		c, err := decodeCursor(opts.Cursor, opts)
		if err != nil {
			return nil, "", err
		}
		after = &c
	}
	if !validID(dirID) {
		return nil, "", ErrNotFound
	}
	dir, err := getNode(ctx, s.db, dirID)
	if err != nil {
		return nil, "", err
	}
	if err := allow(ctx, s.db, actorID, &dir, access.Read); err != nil {
		return nil, "", err
	}
	if dir.Kind != KindDir {
		return nil, "", ErrNotDir
	}

	// One index range per kind: the folders after the cursor, then, if the
	// page isn't full yet, the files from the start.
	var lastKey string
	for _, kind := range []Kind{KindDir, KindFile} {
		if after != nil && after.Kind == KindFile && kind == KindDir {
			continue
		}
		var from *cursor
		if after != nil && after.Kind == kind {
			from = after
		}
		need := opts.Limit - len(items)
		// One extra row tells whether anything follows this page.
		page, keys, err := listChildren(ctx, s.db, dir.ID, kind, opts.Sort, opts.Desc, from, need+1)
		if err != nil {
			return nil, "", err
		}
		for i := range page {
			// Inherited from the folder. A share on a child itself can
			// grant more, which acting on that child will honour.
			page[i].Perms = dir.Perms
		}
		if len(page) > need {
			items = append(items, page[:need]...)
			if need > 0 {
				lastKey = keys[need-1]
			}
			last := items[len(items)-1]
			return items, encodeCursor(cursor{Sort: opts.Sort, Desc: opts.Desc, Kind: last.Kind, Value: cursorValue(opts.Sort, last, lastKey), ID: last.ID}), nil
		}
		items = append(items, page...)
		if len(keys) > 0 {
			lastKey = keys[len(keys)-1]
		}
	}
	return items, "", nil
}

// Entry is a node below the folder Walk started from.
type Entry struct {
	ID string
	// Path is relative to that folder, slash-separated: "Docs/a.txt".
	Path  string
	Kind  Kind
	Size  int64
	MTime time.Time
}

// Walk returns everything below dirID except the trash, parents before
// their children. It is one statement, so one consistent snapshot of the
// subtree, however long acting on it then takes.
func (s *Service) Walk(ctx context.Context, actorID, dirID string) ([]Entry, error) {
	if !validID(dirID) {
		return nil, ErrNotFound
	}
	dir, err := getNode(ctx, s.db, dirID)
	if err != nil {
		return nil, err
	}
	if err := allow(ctx, s.db, actorID, &dir, access.Read); err != nil {
		return nil, err
	}
	if dir.Kind != KindDir {
		return nil, ErrNotDir
	}
	return walk(ctx, s.db, dir.ID)
}

// PathEntry is one folder on the way from the root to a node.
type PathEntry struct {
	ID   string
	Name string // "" for the root
}

// Path returns the chain from the user's root down to id, both included:
// what breadcrumbs show. For a node shared with the actor it starts at the
// shared folder instead.
func (s *Service) Path(ctx context.Context, actorID, id string) ([]PathEntry, error) {
	n, err := s.Get(ctx, actorID, id)
	if err != nil {
		return nil, err
	}
	c, err := chain(ctx, s.db, id)
	if err != nil {
		return nil, fmt.Errorf("path: %w", err)
	}
	path := make([]PathEntry, len(c))
	for i, e := range c {
		path[len(c)-1-i] = PathEntry(e)
	}
	if n.OwnerID == actorID {
		return path, nil
	}
	// A recipient's path starts at the topmost folder shared with them:
	// the owner's folders above it are none of their business.
	ids := make([]string, len(path))
	for i, e := range path {
		ids[i] = e.ID
	}
	roots, err := access.ShareRoots(ctx, s.db, actorID, ids)
	if err != nil {
		return nil, fmt.Errorf("path: %w", err)
	}
	for i, e := range path {
		if roots[e.ID] {
			return path[i:], nil
		}
	}
	return path[len(path)-1:], nil
}
