package fs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/do-service/cloud/backend/internal/access"
)

// ErrQueryTooShort: a substring search needs whole trigrams to use the
// index; shorter queries would scan every name the user has.
var ErrQueryTooShort = errors.New("fs: search query must be at least 3 characters")

const (
	MinSearchQuery     = 3
	DefaultSearchLimit = 50
	MaxSearchLimit     = 200
)

// SearchResult is a node whose name matched, with the folder it is in.
type SearchResult struct {
	Node
	// Path is the folder it is in, from the root: "Docs/2026" ("" for
	// the root itself).
	Path string
}

type searchCursor struct {
	Query string `json:"q"`
	Key   string `json:"k"`
	ID    string `json:"i"`
}

// Search finds the actor's live nodes whose name contains query, ignoring
// case (R5), ordered by name like a listing, a page at a time; cursor and
// next work as in List.
func (s *Service) Search(ctx context.Context, actorID, query string, limit int, cursor string) (items []SearchResult, next string, err error) {
	// Matched against name_key, so normalized the same way.
	q := strings.ToLower(norm.NFC.String(strings.TrimSpace(query)))
	if utf8.RuneCountInString(q) < MinSearchQuery {
		return nil, "", ErrQueryTooShort
	}
	switch {
	case limit <= 0:
		limit = DefaultSearchLimit
	case limit > MaxSearchLimit:
		limit = MaxSearchLimit
	}
	var after *searchCursor
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		var c searchCursor
		if err != nil || json.Unmarshal(raw, &c) != nil || c.Query != q || !validID(c.ID) {
			return nil, "", ErrInvalidCursor
		}
		after = &c
	}
	items, keys, err := searchNodes(ctx, s.db, actorID, likeContains(q), after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("search: %w", err)
	}
	// The actor's own tree only, for now: all theirs.
	for i := range items {
		items[i].Perms = access.All
	}
	if len(items) > limit {
		items = items[:limit]
		// Marshalling these field types can't fail.
		raw, _ := json.Marshal(searchCursor{Query: q, Key: keys[limit-1], ID: items[limit-1].ID})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	return items, next, nil
}

// likeContains is a LIKE pattern matching s anywhere, with LIKE's own
// wildcards in s taken literally: "50%_off" finds that text only.
func likeContains(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}
