package fs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/storage"
)

// Trash is soft delete (R8). Deleting a node marks it and every live node
// below it with one trashed_at, and flags the node itself as trash_root:
// that is what the trash lists, restores and purges. In a tree layout the
// node's content moves to trash/{id} with one rename, so a new file can
// take the old path. Trashed content stays in the user's used_bytes until
// it is purged: it still occupies the disk.
//
// A node trashed earlier inside a folder trashed later keeps its own
// trashed_at and stays a separate trash item; restoring the folder leaves
// it in the trash.

// TrashItem is a node in the trash, with where it was deleted from.
type TrashItem struct {
	Node
	TrashedAt time.Time
	// OriginalPath is the folder it was in, from the root: "Docs/2026"
	// ("" for the root), as it stands now.
	OriginalPath string
}

// maxRestoreNames bounds the "name (2)", "name (3)", … tried on restore.
const maxRestoreNames = 100

func trashKey(id string) string { return "trash/" + id }

// Trash moves id with everything below it to the trash.
func (s *Service) Trash(ctx context.Context, actorID, id string) error {
	return s.TrashIfMatch(ctx, actorID, id, nil)
}

// TrashIfMatch is Trash that fails with ErrPrecondition unless id has one
// of the ETags in ifMatch once locked, as in MoveIfMatch: a sync client
// deletes only what it has seen, a folder with nothing new inside.
func (s *Service) TrashIfMatch(ctx context.Context, actorID, id string, ifMatch []string) error {
	if !validID(id) {
		return ErrNotFound
	}
	return s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		chains, err := lockChains(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		node, err := getNode(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if err := allow(ctx, tx, actorID, &node, access.Read); err != nil {
			return nil, err
		}
		if node.ParentID == "" {
			return nil, ErrIsRoot
		}
		// It lands in the owner's trash, whoever deletes it.
		if err := containerWritable(ctx, tx, actorID, node.ParentID); err != nil {
			return nil, err
		}
		if !(PutCond{IfMatch: ifMatch}).holds(node, true) {
			return nil, ErrPrecondition
		}
		subtree := ""
		if node.Kind == KindDir {
			subtree = id
		}
		if err := checkLocks(ctx, tx, actorID, [][]chainEntry{chains[0], chains[0][1:]}, subtree); err != nil {
			return nil, err
		}
		// Readers resolving a path through it wait until it's gone.
		if err := lockPathExclusive(ctx, tx, id); err != nil {
			return nil, err
		}
		if err := markTrashed(ctx, tx, id); err != nil {
			return nil, err
		}
		if err := bumpAncestors(ctx, tx, node.ParentID, -node.Size); err != nil {
			return nil, err
		}
		// The nodes below go with it, unjournaled: the client drops them
		// with their folder.
		if err := journal(ctx, tx, node.OwnerID, id, opTrash, chains[0]); err != nil {
			return nil, err
		}
		if s.store.Layout() != storage.LayoutTree {
			return nil, nil
		}
		from, to := treeKey(node.OwnerID, chains[0][1:], node.Name), trashKey(id)
		if err := s.store.Move(ctx, from, to); err != nil {
			return nil, err
		}
		return func() error { return s.store.Move(context.WithoutCancel(ctx), to, from) }, nil
	})
}

// ListTrash returns a page of the actor's trash, most recently deleted
// first; cursor and next work as in List.
func (s *Service) ListTrash(ctx context.Context, actorID string, limit int, cursor string) (items []TrashItem, next string, err error) {
	switch {
	case limit <= 0:
		limit = DefaultListLimit
	case limit > MaxListLimit:
		limit = MaxListLimit
	}
	var after *trashCursor
	if cursor != "" {
		c, err := decodeTrashCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		after = &c
	}
	items, err = listTrash(ctx, s.db, actorID, after, limit+1)
	if err != nil {
		return nil, "", err
	}
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = encodeTrashCursor(trashCursor{At: last.TrashedAt, ID: last.ID})
	}
	return items, next, nil
}

// Restore brings a trash item back with its subtree: into its folder if
// that is still live, else into the root; under its name, or "name (2)"
// and so on if the name is taken now.
func (s *Service) Restore(ctx context.Context, actorID, id string) (Node, error) {
	if !validID(id) {
		return Node{}, ErrNotFound
	}
	var restored Node
	err := s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		item, err := lockTrashRoot(ctx, tx, actorID, id)
		if err != nil {
			return nil, err
		}
		target := item.ParentID
		if _, err := getNode(ctx, tx, target); errors.Is(err, ErrNotFound) {
			root, err := getRoot(ctx, tx, item.OwnerID)
			if err != nil {
				return nil, err
			}
			target = root.ID
		} else if err != nil {
			return nil, err
		}
		chains, err := lockChains(ctx, tx, target)
		if err != nil {
			return nil, err
		}
		if err := checkLocks(ctx, tx, actorID, chains[:1], ""); err != nil {
			return nil, err
		}
		// The target chain is locked, so no one adds a name to it before
		// this commits: a free name stays free.
		name, key, err := freeName(ctx, tx, target, item.Name)
		if err != nil {
			return nil, err
		}
		// Placed first, while still trashed: the unique name index only
		// covers live nodes, and the old name may be taken in the old
		// folder.
		if _, err := updatePlacement(ctx, tx, id, target, name, key); err != nil {
			return nil, err
		}
		if err := unmarkTrashed(ctx, tx, id); err != nil {
			return nil, err
		}
		if restored, err = getNode(ctx, tx, id); err != nil {
			return nil, err
		}
		restored.Perms = access.All
		if err := bumpAncestors(ctx, tx, target, item.Size); err != nil {
			return nil, err
		}
		// Where it was doesn't matter: nobody saw it in the trash.
		if err := journal(ctx, tx, item.OwnerID, id, opRestore, chains[0]); err != nil {
			return nil, err
		}
		if s.store.Layout() != storage.LayoutTree {
			return nil, nil
		}
		from, to := trashKey(id), treeKey(item.OwnerID, chains[0], name)
		if err := s.store.Move(ctx, from, to); err != nil {
			return nil, err
		}
		return func() error { return s.store.Move(context.WithoutCancel(ctx), to, from) }, nil
	})
	return restored, err
}

// Purge deletes a trash item for good: its rows, its blobs once nothing
// else references them, its content, and its bytes from the quota.
func (s *Service) Purge(ctx context.Context, actorID, id string) error {
	if !validID(id) {
		return ErrNotFound
	}
	var keys []string
	err := s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		item, err := lockTrashRoot(ctx, tx, actorID, id)
		if err != nil {
			return nil, err
		}
		keys, err = s.purgeRows(ctx, tx, item)
		return nil, err
	})
	if err != nil {
		return err
	}
	s.deleteContent(ctx, keys)
	return nil
}

// EmptyTrash purges every item in the actor's trash, one transaction per
// item, so a huge trash doesn't hold one transaction open throughout.
func (s *Service) EmptyTrash(ctx context.Context, actorID string) error {
	for {
		rows, err := s.db.Query(ctx, `SELECT id FROM nodes WHERE owner_id = $1 AND trash_root LIMIT 100`, actorID)
		if err != nil {
			return fmt.Errorf("list trash: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("list trash: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			// Gone already: purged along with a folder it was in, or by a
			// concurrent request.
			if err := s.Purge(ctx, actorID, id); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
	}
}

// purgeRows deletes item's subtree from the database and returns the
// storage keys left to delete once that commits. Deleting content before
// the commit could lose data a failed commit still references; deleting
// after can at worst leave orphans on disk, which a scan reclaims.
func (s *Service) purgeRows(ctx context.Context, tx pgx.Tx, item Node) ([]string, error) {
	// One snapshot of everything below item, trashed earlier or not:
	// the bytes it frees (current content and versions, as used_bytes
	// counts them), the nested trash items whose content sits in trash/
	// on its own, and every blob reference it drops.
	var bytes int64
	var trashRoots, blobIDs, versioned, users, groups []string
	err := tx.QueryRow(ctx, `
		WITH RECURSIVE t AS (
			SELECT id, kind, size, blob_id, trash_root FROM nodes WHERE id = $1
			UNION ALL
			SELECT n.id, n.kind, n.size, n.blob_id, n.trash_root FROM nodes n JOIN t ON n.parent_id = t.id
		), v AS (
			SELECT versions.node_id, versions.blob_id, versions.size FROM versions JOIN t ON versions.node_id = t.id
		)
		SELECT
			(SELECT coalesce(sum(size), 0) FROM t WHERE kind = 'file') + (SELECT coalesce(sum(size), 0) FROM v),
			ARRAY(SELECT id::text FROM t WHERE trash_root),
			ARRAY(SELECT blob_id::text FROM t WHERE blob_id IS NOT NULL UNION ALL SELECT blob_id::text FROM v),
			ARRAY(SELECT DISTINCT node_id::text FROM v),
			ARRAY(SELECT DISTINCT grantee_id::text FROM shares JOIN t ON shares.node_id = t.id WHERE grantee_type = 'user'),
			ARRAY(SELECT DISTINCT grantee_id::text FROM shares JOIN t ON shares.node_id = t.id WHERE grantee_type = 'group')`,
		item.ID).Scan(&bytes, &trashRoots, &blobIDs, &versioned, &users, &groups)
	if err != nil {
		return nil, fmt.Errorf("purge: collect subtree: %w", err)
	}
	// The cascade takes the subtree and its versions.
	if _, err := tx.Exec(ctx, `DELETE FROM nodes WHERE id = $1`, item.ID); err != nil {
		return nil, fmt.Errorf("purge: delete nodes: %w", err)
	}
	keys, err := releaseBlobs(ctx, tx, blobIDs)
	if err != nil {
		return nil, err
	}
	if err := addUsedBytes(ctx, tx, item.OwnerID, -bytes); err != nil {
		return nil, err
	}
	// The cascade took the shares below too: their recipients' view
	// changed (the owner's account deletion purges through here as well).
	if err := TouchAccess(ctx, tx, users, groups); err != nil {
		return nil, err
	}
	if s.store.Layout() == storage.LayoutTree {
		// The current content of everything purged sits under these, and
		// each file's versions in a directory of its own.
		for _, id := range trashRoots {
			keys = append(keys, trashKey(id))
		}
		for _, id := range versioned {
			keys = append(keys, "versions/"+id)
		}
	}
	return keys, nil
}

func (s *Service) deleteContent(ctx context.Context, keys []string) {
	for _, key := range keys {
		if err := s.store.Delete(context.WithoutCancel(ctx), key); err != nil {
			slog.ErrorContext(ctx, "fs: purged content left in storage", "key", key, "err", err)
		}
	}
}

// freeName returns name, or the first "name (n)" not taken by a live
// child of parentID.
func freeName(ctx context.Context, q querier, parentID, name string) (string, string, error) {
	ext := path.Ext(name)
	if ext == name {
		ext = "" // ".bashrc" is all name, no extension
	}
	base := strings.TrimSuffix(name, ext)
	for i := 1; i <= maxRestoreNames; i++ {
		candidate := name
		if i > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		n, key, err := NormalizeName(candidate)
		if err != nil {
			// Suffixing ran past the length limit.
			return "", "", ErrNameConflict
		}
		var taken bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM nodes WHERE parent_id = $1 AND name_key = $2 AND trashed_at IS NULL)`, parentID, key).Scan(&taken); err != nil {
			return "", "", err
		}
		if !taken {
			return n, key, nil
		}
	}
	return "", "", ErrNameConflict
}

type trashCursor struct {
	At time.Time `json:"t"`
	ID string    `json:"i"`
}

func encodeTrashCursor(c trashCursor) string {
	// Marshalling these field types can't fail.
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeTrashCursor(s string) (trashCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	var c trashCursor
	if err != nil || json.Unmarshal(raw, &c) != nil || !validID(c.ID) || c.At.IsZero() {
		return trashCursor{}, ErrInvalidCursor
	}
	return c, nil
}

// TrashRef names a trash item and its owner, for jobs acting on anyone's.
type TrashRef struct {
	ID      string
	OwnerID string
}

// ExpiredTrash returns up to limit trash items deleted before cutoff,
// oldest first: what the trash retention purges (E4-4).
func (s *Service) ExpiredTrash(ctx context.Context, cutoff time.Time, limit int) ([]TrashRef, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id::text, owner_id::text FROM nodes
		WHERE trash_root AND trashed_at < $1 ORDER BY trashed_at LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("expired trash: %w", err)
	}
	refs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[TrashRef])
	if err != nil {
		return nil, fmt.Errorf("expired trash: %w", err)
	}
	return refs, nil
}
