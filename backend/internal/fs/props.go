package fs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/do-service/cloud/backend/internal/access"
)

// Dead properties (RFC 4918 §4): what WebDAV clients store on a node by
// PROPPATCH, kept as given. The protocol layer decides which names are
// live (computed) and never stores those.

// Limits on what one node may carry: clients store timestamps and small
// flags, not documents.
const (
	MaxPropValueBytes = 16 << 10
	MaxPropsPerNode   = 64
)

// ErrPropsTooLarge: a value or the node's property count over the limits.
var ErrPropsTooLarge = errors.New("fs: properties too large")

// Prop is a dead property. Value is its content as self-contained XML.
type Prop struct {
	Space, Local string
	Value        string
}

// PropChange sets a property, or with Remove removes it.
type PropChange struct {
	Prop
	Remove bool
}

// Props returns the dead properties of id and, with children, of each of
// its children too (a PROPFIND with Depth 1), by node id.
func (s *Service) Props(ctx context.Context, actorID, id string, children bool) (map[string][]Prop, error) {
	if _, err := s.Get(ctx, actorID, id); err != nil {
		return nil, err
	}
	q := `SELECT node_id::text, namespace, name, value FROM dav_props WHERE node_id = $1`
	if children {
		// What's in a folder one may read is readable too.
		q += ` OR node_id IN (SELECT id FROM nodes WHERE parent_id = $1 AND trashed_at IS NULL)`
	}
	rows, err := s.db.Query(ctx, q+` ORDER BY namespace, name`, id)
	if err != nil {
		return nil, fmt.Errorf("props: %w", err)
	}
	defer rows.Close()
	props := map[string][]Prop{}
	for rows.Next() {
		var nodeID string
		var p Prop
		if err := rows.Scan(&nodeID, &p.Space, &p.Local, &p.Value); err != nil {
			return nil, fmt.Errorf("props: %w", err)
		}
		props[nodeID] = append(props[nodeID], p)
	}
	return props, rows.Err()
}

// PatchProps applies changes to the dead properties of id in order, all
// or nothing (RFC 4918 §9.2). Removing one that isn't there is no error.
// It takes write on the node.
func (s *Service) PatchProps(ctx context.Context, actorID, id string, changes []PropChange) error {
	for _, p := range changes {
		if len(p.Value) > MaxPropValueBytes {
			return ErrPropsTooLarge
		}
	}
	if !validID(id) {
		return ErrNotFound
	}
	return s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		// The row lock orders concurrent patches of one node, so the count
		// below can't be exceeded by two of them together.
		n, err := scanNode(tx.QueryRow(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = $1 AND trashed_at IS NULL FOR UPDATE`, id))
		if err != nil {
			return nil, err
		}
		if err := allow(ctx, tx, actorID, &n, access.Write); err != nil {
			return nil, err
		}
		c, err := chain(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if err := checkLocks(ctx, tx, actorID, [][]chainEntry{c}, ""); err != nil {
			return nil, err
		}
		for _, p := range changes {
			if p.Remove {
				if _, err := tx.Exec(ctx, `DELETE FROM dav_props WHERE node_id = $1 AND namespace = $2 AND name = $3`, id, p.Space, p.Local); err != nil {
					return nil, err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO dav_props (node_id, namespace, name, value) VALUES ($1, $2, $3, $4)
				ON CONFLICT (node_id, namespace, name) DO UPDATE SET value = excluded.value`, id, p.Space, p.Local, p.Value); err != nil {
				return nil, err
			}
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dav_props WHERE node_id = $1`, id).Scan(&count); err != nil {
			return nil, err
		}
		if count > MaxPropsPerNode {
			return nil, ErrPropsTooLarge
		}
		return nil, nil
	})
}
