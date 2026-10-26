package fs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"

	"github.com/jackc/pgx/v5"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/storage"
)

// MaxCopyNodes bounds a folder copied within one request (WebDAV's COPY,
// small copies from the web UI). Larger ones go to a background job,
// which copies without a bound.
const MaxCopyNodes = 10_000

// ErrCopyTooLarge: the folder has more than MaxCopyNodes nodes.
var ErrCopyTooLarge = errors.New("fs: folder too large to copy at once")

// CopyOptions say how Copy treats a name and a size.
type CopyOptions struct {
	// Rename: a name taken in the destination gets "name (2)" and on,
	// instead of failing with ErrNameConflict.
	Rename bool
	// Unbounded lifts MaxCopyNodes, for a background job.
	Unbounded bool
}

// Copy duplicates id into destParentID as name ("" keeps its name): a file
// with its current content, a folder with everything in it. The copy
// counts against the destination owner's quota. It is not atomic: a
// folder is copied node by node, and one that fails halfway is removed.
// Content is copied by storage — on Linux by the kernel, as a reflink
// where the filesystem has them — and its hash taken over from the
// source, so nothing is read through or hashed again.
func (s *Service) Copy(ctx context.Context, actorID, id, destParentID, name string, opts CopyOptions) (Node, error) {
	src, err := s.Get(ctx, actorID, id)
	if err != nil {
		return Node{}, err
	}
	if name == "" {
		name = src.Name
	}
	if !validID(destParentID) {
		return Node{}, ErrNotFound
	}
	dest, err := writableDir(ctx, s.db, actorID, destParentID)
	if err != nil {
		return Node{}, err
	}
	if src.Kind == KindFile {
		return s.copyFile(ctx, actorID, src.ID, dest.ID, name, opts.Rename)
	}

	destChain, err := chain(ctx, s.db, dest.ID)
	if err != nil {
		return Node{}, err
	}
	for _, e := range destChain {
		if e.ID == src.ID {
			return Node{}, ErrCycle
		}
	}
	entries, err := s.Walk(ctx, actorID, src.ID)
	if err != nil {
		return Node{}, err
	}
	if len(entries) >= MaxCopyNodes && !opts.Unbounded {
		return Node{}, ErrCopyTooLarge
	}
	if err := s.fitsQuota(ctx, dest.OwnerID, src.Size); err != nil {
		return Node{}, err
	}
	root, err := s.createDir(ctx, actorID, dest.ID, name, opts.Rename)
	if err != nil {
		return Node{}, err
	}
	// Walk lists folders before their contents, so each parent is there.
	ids := map[string]string{".": root.ID}
	for _, e := range entries {
		parent, base := ids[path.Dir(e.Path)], path.Base(e.Path)
		var n Node
		if e.Kind == KindDir {
			n, err = s.CreateDir(ctx, actorID, parent, base)
		} else {
			n, err = s.copyFile(ctx, actorID, e.ID, parent, base, false)
		}
		if err != nil {
			s.discard(ctx, actorID, root.ID)
			return Node{}, err
		}
		ids[e.Path] = n.ID
	}
	return root, nil
}

// CopyWork is how big a copy of id would be: its bytes, and whether it
// has more than limit nodes below — counted only that far, so a huge
// folder costs no more than a small one to ask about.
func (s *Service) CopyWork(ctx context.Context, actorID, id string, limit int) (bytes int64, over bool, err error) {
	n, err := s.Get(ctx, actorID, id)
	if err != nil {
		return 0, false, err
	}
	if n.Kind == KindFile {
		return n.Size, false, nil
	}
	var count int
	err = s.db.QueryRow(ctx, `
		SELECT count(*) FROM (
			WITH RECURSIVE t AS (
				SELECT id, kind FROM nodes WHERE parent_id = $1 AND trashed_at IS NULL
				UNION ALL
				SELECT n.id, n.kind FROM nodes n JOIN t ON n.parent_id = t.id
				WHERE t.kind = 'dir' AND n.trashed_at IS NULL
			)
			SELECT 1 FROM t LIMIT $2
		) c`, n.ID, limit+1).Scan(&count)
	return n.Size, count > limit, err
}

func (s *Service) copyFile(ctx context.Context, actorID, id, destParentID, name string, rename bool) (Node, error) {
	if _, _, err := NormalizeName(name); err != nil {
		return Node{}, err
	}
	st, err := s.copyContent(ctx, actorID, id)
	if err != nil {
		return Node{}, err
	}
	onConflict := ConflictFail
	if rename {
		onConflict = ConflictRename
	}
	return s.commitFile(ctx, actorID, destParentID, name, st, onConflict, func(tx pgx.Tx) error {
		owner, err := getNode(ctx, tx, destParentID)
		if err != nil {
			return err
		}
		return checkQuota(ctx, tx, owner.OwnerID, st.size)
	})
}

// copyContent copies file id's current content to a new staging key, with
// the size and hash of the blob it is: what storage copied is that blob.
// In a tree layout the content's key is its path, resolved and copied
// under shared locks on the chain, as OpenRange reads it.
func (s *Service) copyContent(ctx context.Context, actorID, id string) (staged, error) {
	blobID := newUUID()
	st := staged{key: "uploads/fs-" + blobID, blobID: blobID}
	if s.store.Layout() != storage.LayoutTree {
		st.key = flatKey(blobID)
	}
	var src Node
	err := func() error {
		if s.store.Layout() != storage.LayoutTree {
			n, err := s.Get(ctx, actorID, id)
			if err != nil {
				return err
			}
			if n.Kind != KindFile {
				return ErrNotFile
			}
			src = n
			return s.store.Copy(ctx, flatKey(n.blobID), st.key)
		}
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = rollback(tx) }()
		c, err := lockPathShared(ctx, tx, id)
		if err != nil {
			return err
		}
		n, err := getNode(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := allow(ctx, tx, actorID, &n, access.Read); err != nil {
			return err
		}
		if n.Kind != KindFile {
			return ErrNotFile
		}
		src = n
		if err := s.store.Copy(ctx, treeKey(n.OwnerID, c[1:], n.Name), st.key); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}()
	if err != nil {
		if delErr := s.store.Delete(context.WithoutCancel(ctx), st.key); delErr != nil {
			slog.ErrorContext(ctx, "fs: remove orphaned content", "key", st.key, "err", delErr)
		}
		return staged{}, fmt.Errorf("copy content: %w", err)
	}
	st.size = src.Size
	copy(st.sha256[:], src.ContentHash)
	return st, nil
}

// discard removes a half-made copy: to the trash, then for good. Purging
// is the owner's; copied into someone else's folder, it stays in their
// trash.
func (s *Service) discard(ctx context.Context, actorID, id string) {
	ctx = context.WithoutCancel(ctx)
	err := s.Trash(ctx, actorID, id)
	if err == nil {
		err = s.Purge(ctx, actorID, id)
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		slog.ErrorContext(ctx, "fs: remove an unfinished copy", "node", id, "err", err)
	}
}
