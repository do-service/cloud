package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/storage"
)

// Versions (R7): replacing a file's content keeps the old content as a
// version of it. In a tree layout the old content moves, with one rename,
// from the file's path to versions/{node_id}/{version_id}; its blob keeps
// it there under that storage key. Versions count in the owner's quota
// until a retention policy drops them (E4-2) or the file is purged.

// Version is an earlier content of a file.
type Version struct {
	ID     string
	NodeID string
	Size   int64
	Hash   []byte
	// CreatedAt is when this content was saved: the file's mtime back
	// when it was current.
	CreatedAt time.Time
	// Author saved this content; zero when unknown.
	Author Author

	storageKey string
}

// Author is the user who wrote some content. A zero Author is unknown: the
// content predates authors being kept, or the account was deleted.
type Author struct {
	ID string
	// Name is the display name, or the username when there's none.
	Name string
}

// authorOf is who an actor's writes are credited to: no one for a public
// link, whose uploader isn't a user.
func authorOf(actorID string) string {
	if _, ok := access.LinkID(actorID); ok {
		return ""
	}
	return actorID
}

// authorColumns reads an Author from the users row u, which may be missing.
const authorColumns = `coalesce(u.id::text, ''), coalesce(nullif(u.display_name, ''), u.username::text, '')`

func versionKey(nodeID, versionID string) string { return "versions/" + nodeID + "/" + versionID }

// replaceContent makes st the content of the file node, in the folder whose
// locked chain is given (folder first), keeping the current content as a
// version. actorID becomes the author of the new content, and whoever wrote
// the old one that of the version. It returns the updated node and how to
// undo the storage moves.
func (s *Service) replaceContent(ctx context.Context, tx pgx.Tx, actorID string, node Node, folder []chainEntry, st staged) (Node, func() error, error) {
	// Readers resolving the path wait while the content under it changes.
	if err := lockPathExclusive(ctx, tx, node.ID); err != nil {
		return Node{}, nil, err
	}
	tree := s.store.Layout() == storage.LayoutTree
	versionID := newUUID()
	current, archived := treeKey(node.OwnerID, folder, node.Name), versionKey(node.ID, versionID)
	if tree {
		// A flat layout's blob keeps its own key; a tree layout's moves
		// out of the file's path.
		if _, err := tx.Exec(ctx, `UPDATE blobs SET storage_key = $2 WHERE id = $1`, node.blobID, archived); err != nil {
			return Node{}, nil, err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO versions (id, node_id, blob_id, size, hash, created_at, author_id)
		SELECT $1, n.id, n.blob_id, n.size, b.hash, n.mtime, n.modified_by FROM nodes n JOIN blobs b ON b.id = n.blob_id WHERE n.id = $2`,
		versionID, node.ID); err != nil {
		return Node{}, nil, fmt.Errorf("keep version: %w", err)
	}
	backend, storageKey := "localfs", ""
	if !tree {
		backend, storageKey = "s3", st.key
	}
	if err := insertBlob(ctx, tx, st.blobID, backend, storageKey, st.size, st.sha256[:]); err != nil {
		return Node{}, nil, err
	}
	updated, err := scanNode(tx.QueryRow(ctx, `
		UPDATE nodes SET blob_id = $2, size = $3, content_hash = $4, mime = nullif($5, ''), mtime = coalesce($7, now()),
			etag = replace(gen_random_uuid()::text, '-', ''), updated_at = now(), modified_by = nullif($6, '')::uuid
		WHERE id = $1 RETURNING `+nodeColumns, node.ID, st.blobID, st.size, st.sha256[:], mimeType(node.Name), authorOf(actorID), orNow(st.mtime)))
	if err != nil {
		return Node{}, nil, err
	}
	if err := bumpAncestors(ctx, tx, node.ParentID, st.size-node.Size); err != nil {
		return Node{}, nil, err
	}
	// The old content stays in the quota as a version.
	if err := addUsedBytes(ctx, tx, node.OwnerID, st.size); err != nil {
		return Node{}, nil, err
	}
	if err := journal(ctx, tx, node.OwnerID, node.ID, opContent, folder); err != nil {
		return Node{}, nil, err
	}
	if !tree {
		return updated, nil, nil
	}
	if err := s.store.Move(ctx, current, archived); err != nil {
		return Node{}, nil, err
	}
	if err := s.store.Move(ctx, st.key, current); err != nil {
		if undoErr := s.store.Move(context.WithoutCancel(ctx), archived, current); undoErr != nil {
			slog.ErrorContext(ctx, "fs: version move could not be undone", "key", archived, "err", undoErr)
		}
		return Node{}, nil, err
	}
	undo := func() error {
		ctx := context.WithoutCancel(ctx)
		return errors.Join(s.store.Move(ctx, current, st.key), s.store.Move(ctx, archived, current))
	}
	return updated, undo, nil
}

// ListVersions returns a file's earlier contents, newest first, and the
// author of its current one, which is the file itself.
func (s *Service) ListVersions(ctx context.Context, actorID, nodeID string) ([]Version, Author, error) {
	n, err := s.Get(ctx, actorID, nodeID)
	if err != nil {
		return nil, Author{}, err
	}
	if n.Kind != KindFile {
		return nil, Author{}, ErrNotFile
	}
	var current Author
	if err := s.db.QueryRow(ctx, `SELECT `+authorColumns+` FROM nodes n LEFT JOIN users u ON u.id = n.modified_by WHERE n.id = $1`, n.ID).
		Scan(&current.ID, &current.Name); err != nil {
		return nil, Author{}, fmt.Errorf("list versions: %w", err)
	}
	rows, err := s.db.Query(ctx, `
		SELECT v.id, v.node_id, v.size, v.hash, v.created_at, coalesce(b.storage_key, ''), `+authorColumns+`
		FROM versions v JOIN blobs b ON b.id = v.blob_id LEFT JOIN users u ON u.id = v.author_id
		WHERE v.node_id = $1 ORDER BY v.created_at DESC, v.id DESC`, n.ID)
	if err != nil {
		return nil, Author{}, fmt.Errorf("list versions: %w", err)
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Version, error) {
		var v Version
		err := row.Scan(&v.ID, &v.NodeID, &v.Size, &v.Hash, &v.CreatedAt, &v.storageKey, &v.Author.ID, &v.Author.Name)
		return v, err
	})
	if err != nil {
		return nil, Author{}, fmt.Errorf("list versions: %w", err)
	}
	return versions, current, nil
}

// getVersion returns versionID with the live file it belongs to, if the
// actor may see that file.
func (s *Service) getVersion(ctx context.Context, actorID, versionID string) (Version, Node, error) {
	if !validID(versionID) {
		return Version{}, Node{}, ErrNotFound
	}
	var v Version
	err := s.db.QueryRow(ctx, `
		SELECT v.id, v.node_id, v.size, v.hash, v.created_at, coalesce(b.storage_key, '')
		FROM versions v JOIN blobs b ON b.id = v.blob_id WHERE v.id = $1`, versionID).
		Scan(&v.ID, &v.NodeID, &v.Size, &v.Hash, &v.CreatedAt, &v.storageKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, Node{}, ErrNotFound
	}
	if err != nil {
		return Version{}, Node{}, err
	}
	n, err := s.Get(ctx, actorID, v.NodeID)
	return v, n, err
}

// OpenVersion opens an earlier content of a file, like OpenRange does the
// current one. pick gets the file node as that version would make it:
// its size, its time, and the version id as a never-changing ETag.
func (s *Service) OpenVersion(ctx context.Context, actorID, versionID string, pick func(Node) (*storage.Range, error)) (io.ReadCloser, Node, error) {
	v, n, err := s.getVersion(ctx, actorID, versionID)
	if err != nil {
		return nil, Node{}, err
	}
	n.Size, n.MTime, n.ETag, n.ContentHash = v.Size, v.CreatedAt, v.ID, v.Hash
	rng, err := pick(n)
	if err != nil {
		return nil, Node{}, err
	}
	// The key doesn't depend on the file's path, so no path locks.
	rc, err := s.store.Get(ctx, v.storageKey, rng)
	return rc, n, err
}

// RestoreVersion makes an earlier content current again. The current one
// becomes a version in turn, so restoring adds to the history instead of
// rewriting it (R7). The content is copied: the version keeps its own.
func (s *Service) RestoreVersion(ctx context.Context, actorID, versionID string) (Node, error) {
	v, _, err := s.getVersion(ctx, actorID, versionID)
	if err != nil {
		return Node{}, err
	}
	st := staged{blobID: newUUID(), size: v.Size}
	copy(st.sha256[:], v.Hash)
	st.key = "uploads/fs-" + st.blobID
	if s.store.Layout() != storage.LayoutTree {
		st.key = flatKey(st.blobID)
	}
	// Copied outside the transaction: a big file takes a while.
	if err := s.store.Copy(ctx, v.storageKey, st.key); err != nil {
		return Node{}, fmt.Errorf("copy version: %w", err)
	}
	var restored Node
	err = s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		chains, err := lockChains(ctx, tx, v.NodeID)
		if err != nil {
			return nil, err
		}
		n, err := getNode(ctx, tx, v.NodeID)
		if err != nil {
			return nil, err
		}
		if err := allow(ctx, tx, actorID, &n, access.Write); err != nil {
			return nil, err
		}
		if err := checkLocks(ctx, tx, actorID, chains[:1], ""); err != nil {
			return nil, err
		}
		restored, undo, err = s.replaceContent(ctx, tx, actorID, n, chains[0][1:], st)
		restored.Perms = n.Perms
		return undo, err
	})
	if err != nil {
		if delErr := s.store.Delete(context.WithoutCancel(ctx), st.key); delErr != nil {
			slog.ErrorContext(ctx, "fs: remove orphaned content", "key", st.key, "err", delErr)
		}
		return Node{}, err
	}
	return restored, nil
}

// VersionPolicy is how much history a file keeps (E4-2). Zero values mean
// no limit.
type VersionPolicy struct {
	// MaxCount keeps a file's newest versions only.
	MaxCount int
	// MaxAge drops versions saved longer ago than this.
	MaxAge time.Duration
}

// PruneVersions deletes up to limit versions the policy no longer keeps,
// with their content, and gives their bytes back to the owners' quota. It
// returns how many it deleted; a caller with more to do runs it again.
func (s *Service) PruneVersions(ctx context.Context, p VersionPolicy, limit int) (int, error) {
	if p.MaxCount <= 0 && p.MaxAge <= 0 {
		return 0, nil
	}
	var keys []string
	var pruned int
	err := s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		// Ranked newest first per file; the age bound is a cutoff time so
		// the statement keeps one plan whatever the policy.
		var cutoff *time.Time
		if p.MaxAge > 0 {
			t := time.Now().Add(-p.MaxAge)
			cutoff = &t
		}
		rows, err := tx.Query(ctx, `
			WITH ranked AS (
				SELECT id, replaced_at, row_number() OVER (PARTITION BY node_id ORDER BY replaced_at DESC, id DESC) AS rn
				FROM versions
			), doomed AS (
				-- By when each was replaced, not created_at: that is the
				-- content's mtime, which a client may set far in the past.
				SELECT id FROM ranked
				WHERE ($1 > 0 AND rn > $1) OR ($2::timestamptz IS NOT NULL AND replaced_at < $2)
				LIMIT $3
			)
			DELETE FROM versions v USING doomed, nodes n
			WHERE v.id = doomed.id AND n.id = v.node_id
			RETURNING v.blob_id::text, v.size, n.owner_id::text`, p.MaxCount, cutoff, limit)
		if err != nil {
			return nil, fmt.Errorf("prune versions: %w", err)
		}
		var blobIDs []string
		freed := map[string]int64{}
		var blobID, owner string
		var size int64
		if _, err := pgx.ForEachRow(rows, []any{&blobID, &size, &owner}, func() error {
			blobIDs = append(blobIDs, blobID)
			freed[owner] += size
			return nil
		}); err != nil {
			return nil, fmt.Errorf("prune versions: %w", err)
		}
		pruned = len(blobIDs)
		if keys, err = releaseBlobs(ctx, tx, blobIDs); err != nil {
			return nil, err
		}
		for owner, n := range freed {
			if err := addUsedBytes(ctx, tx, owner, -n); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	// After the commit, as purging does: a failed commit must still find
	// the content it references.
	s.deleteContent(ctx, keys)
	return pruned, nil
}
