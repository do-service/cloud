// Package access is the one place that decides what a user may do with a
// node (architecture §6). fs asks it on every operation, so no protocol on
// top (REST, WebDAV, WOPI) can skip the check.
//
// The owner may do anything. Anyone else has the union of the shares on
// the node's path from the root (inherited down the tree) that name them
// or one of their groups. Nothing is cached: a revoked share stops working
// on the next request.
package access

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Perm is a set of rights, as stored in shares.perms.
type Perm uint8

const (
	Read Perm = 1 << iota
	// Write: create, rename, move, delete and upload inside; replace and
	// restore file content.
	Write
	// Reshare: share onward, never more than one's own rights.
	Reshare

	None Perm = 0
	All       = Read | Write | Reshare
)

// Has reports whether p includes every right in need.
func (p Perm) Has(need Perm) bool { return p&need == need }

// Querier is what both a pool and a transaction offer.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// linkPrefix marks a principal that is a public link, not a user: the
// rest is the link's id.
const linkPrefix = "link:"

// LinkPrincipal is the principal acting through a public link, passed
// wherever a user id goes.
func LinkPrincipal(linkID string) string { return linkPrefix + linkID }

// LinkID returns the link a principal acts through, if it is one. A
// malformed id still says "a link", one that matches nothing.
func LinkID(principal string) (string, bool) {
	return strings.CutPrefix(principal, linkPrefix)
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Perms returns userID's rights on nodeID, owned by ownerID: All for the
// owner without a query, else what the shares on the path grant. For a
// link principal it is the link's mode, within its subtree, until it
// expires.
func Perms(ctx context.Context, q Querier, userID, nodeID, ownerID string) (Perm, error) {
	if userID == ownerID {
		return All, nil
	}
	if linkID, ok := LinkID(userID); ok {
		return linkPerms(ctx, q, linkID, nodeID)
	}
	var p int16
	err := q.QueryRow(ctx, `
		WITH RECURSIVE path AS (
			SELECT id, parent_id FROM nodes WHERE id = $1
			UNION ALL
			SELECT n.id, n.parent_id FROM nodes n JOIN path ON n.id = path.parent_id
		)
		SELECT coalesce(bit_or(s.perms), 0) FROM shares s JOIN path ON s.node_id = path.id
		WHERE (s.grantee_type = 'user' AND s.grantee_id = $2)
		   OR (s.grantee_type = 'group' AND s.grantee_id IN (SELECT group_id FROM group_members WHERE user_id = $2))`,
		nodeID, userID).Scan(&p)
	return Perm(p), err
}

// linkPerms is what a link grants on nodeID: by its mode if the node is
// the link's or below it, nothing otherwise or once it expired.
func linkPerms(ctx context.Context, q Querier, linkID, nodeID string) (Perm, error) {
	if !uuidPattern.MatchString(linkID) {
		return None, nil
	}
	var mode string
	err := q.QueryRow(ctx, `
		WITH RECURSIVE path AS (
			SELECT id, parent_id FROM nodes WHERE id = $1
			UNION ALL
			SELECT n.id, n.parent_id FROM nodes n JOIN path ON n.id = path.parent_id
		)
		SELECT mode FROM links
		WHERE id = $2 AND (expires_at IS NULL OR expires_at > now()) AND node_id IN (SELECT id FROM path)`,
		nodeID, linkID).Scan(&mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return None, nil
	}
	if err != nil {
		return None, err
	}
	switch mode {
	case "view", "download":
		return Read, nil
	case "upload":
		// A file drop: put files in, see nothing.
		return Write, nil
	}
	return None, nil
}

// ShareRoots returns, among nodeIDs, those carrying a share to userID or
// their groups: where a recipient's view of someone else's tree begins.
// For a link principal it is the link's node.
func ShareRoots(ctx context.Context, q Querier, userID string, nodeIDs []string) (map[string]bool, error) {
	if linkID, ok := LinkID(userID); ok {
		if !uuidPattern.MatchString(linkID) {
			return map[string]bool{}, nil
		}
		var root string
		if err := q.QueryRow(ctx, `SELECT node_id::text FROM links WHERE id = $1`, linkID).Scan(&root); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return map[string]bool{}, nil
			}
			return nil, err
		}
		return map[string]bool{root: true}, nil
	}
	rows, err := q.Query(ctx, `
		SELECT DISTINCT node_id::text FROM shares
		WHERE node_id = ANY($1::uuid[]) AND (
			(grantee_type = 'user' AND grantee_id = $2)
			OR (grantee_type = 'group' AND grantee_id IN (SELECT group_id FROM group_members WHERE user_id = $2)))`,
		nodeIDs, userID)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	roots := make(map[string]bool, len(ids))
	for _, id := range ids {
		roots[id] = true
	}
	return roots, nil
}

// Granted returns every node carrying a share to userID or their groups,
// by owner, with the rights granted right there (not inherited): all of
// others' trees that concern the user, which a sync client follows. A
// trashed node is included, so its trashing still reaches the recipient.
// A link principal is granted nothing here: links don't sync.
func Granted(ctx context.Context, q Querier, userID string) (map[string]map[string]Perm, error) {
	granted := map[string]map[string]Perm{}
	if _, ok := LinkID(userID); ok || !uuidPattern.MatchString(userID) {
		return granted, nil
	}
	rows, err := q.Query(ctx, `
		SELECT n.owner_id::text, s.node_id::text, bit_or(s.perms) FROM shares s JOIN nodes n ON n.id = s.node_id
		WHERE n.owner_id <> $1 AND (
			(s.grantee_type = 'user' AND s.grantee_id = $1)
			OR (s.grantee_type = 'group' AND s.grantee_id IN (SELECT group_id FROM group_members WHERE user_id = $1)))
		GROUP BY n.owner_id, s.node_id`, userID)
	if err != nil {
		return nil, err
	}
	var owner, node string
	var p int16
	_, err = pgx.ForEachRow(rows, []any{&owner, &node, &p}, func() error {
		if granted[owner] == nil {
			granted[owner] = map[string]Perm{}
		}
		granted[owner][node] = Perm(p)
		return nil
	})
	return granted, err
}
