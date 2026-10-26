package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/storage"
)

// ErrPrecondition: a conditional write's condition doesn't hold.
var ErrPrecondition = errors.New("fs: precondition failed")

// PutCond is what a write requires of the file it would replace, as HTTP
// conditional requests state it (RFC 9110 §13.1). ETags are unquoted; "*"
// stands for any existing file. Empty lists require nothing.
type PutCond struct {
	// IfMatch: the file exists with one of these ETags.
	IfMatch []string
	// IfNoneMatch: the file has none of these ETags; with "*", the name
	// must be free.
	IfNoneMatch []string
	// Rename: a taken name doesn't replace the file there, the new one is
	// stored beside it as "name (2).ext" ("Save as" in an editor).
	Rename bool
}

func (c PutCond) holds(existing Node, exists bool) bool {
	matches := func(tags []string) bool {
		return exists && (slices.Contains(tags, "*") || slices.Contains(tags, existing.ETag))
	}
	if len(c.IfMatch) > 0 && !matches(c.IfMatch) {
		return false
	}
	return len(c.IfNoneMatch) == 0 || !matches(c.IfNoneMatch)
}

// PutFile writes r as the file name in parentID: a new file, or a new
// version of the one there (R7) — the WebDAV PUT. size is -1 when unknown.
// cond is checked against the file both before the content is streamed
// and, decisively, under the folder's lock when it is committed: a sync
// client's If-Match must not silently overwrite someone else's change made
// meanwhile. created tells a new file from a replaced one.
func (s *Service) PutFile(ctx context.Context, actorID, parentID, name string, r io.Reader, size int64, cond PutCond) (n Node, created bool, err error) {
	if !validID(parentID) {
		return Node{}, false, ErrNotFound
	}
	_, key, err := NormalizeName(name)
	if err != nil {
		return Node{}, false, err
	}
	check := func(q querier, parentID string) error {
		if cond.Rename {
			created = true
			return nil
		}
		existing, err := liveChild(ctx, q, parentID, key)
		exists := err == nil
		switch {
		case err != nil && !errors.Is(err, ErrNotFound):
			return err
		case exists && existing.Kind != KindFile:
			return ErrNameConflict
		case !cond.holds(existing, exists):
			return ErrPrecondition
		}
		created = !exists
		return nil
	}
	// The locks are checked again when committing; this saves streaming
	// content that couldn't be stored anyway.
	early := func(parentID string) error {
		c, err := chain(ctx, s.db, parentID)
		if err != nil {
			return err
		}
		if existing, err := liveChild(ctx, s.db, parentID, key); err == nil && !cond.Rename {
			c = append([]chainEntry{{ID: existing.ID, Name: existing.Name}}, c...)
		}
		return checkLocks(ctx, s.db, actorID, [][]chainEntry{c}, "")
	}
	// Fail before streaming possibly gigabytes into storage.
	parent, err := writableDir(ctx, s.db, actorID, parentID)
	if err != nil {
		return Node{}, false, err
	}
	if err := check(s.db, parent.ID); err != nil {
		return Node{}, false, err
	}
	if err := early(parent.ID); err != nil {
		return Node{}, false, err
	}
	if size >= 0 {
		if err := s.fitsQuota(ctx, parent.OwnerID, size); err != nil {
			return Node{}, false, err
		}
	}

	blobID := newUUID()
	stagingKey := "uploads/fs-" + blobID
	if s.store.Layout() != storage.LayoutTree {
		stagingKey = flatKey(blobID)
	}
	res, err := s.store.Put(ctx, stagingKey, r, size)
	if err != nil {
		if errors.Is(err, storage.ErrSizeMismatch) {
			return Node{}, false, ErrSizeMismatch
		}
		return Node{}, false, fmt.Errorf("store content: %w", err)
	}
	st := staged{key: stagingKey, blobID: blobID, size: res.Size, sha256: res.SHA256}
	onConflict := ConflictReplace
	if cond.Rename {
		onConflict = ConflictRename
	}
	n, err = s.commitFile(ctx, actorID, parent.ID, name, st, onConflict, func(tx pgx.Tx) error {
		if err := check(tx, parent.ID); err != nil {
			return err
		}
		// A replaced file's old content stays as a version: the new
		// content counts in full.
		return checkQuota(ctx, tx, parent.OwnerID, st.size)
	})
	return n, created, err
}

// WriteFile replaces the content of file id, keeping the current one as a
// version (R7): what an online editor's save does. Unlike PutFile it needs
// write on the file alone, not on its folder, so a file shared on its own
// can be edited. cond is checked against the file; cond.Rename is
// meaningless here.
func (s *Service) WriteFile(ctx context.Context, actorID, id string, r io.Reader, size int64, cond PutCond) (Node, error) {
	n, err := s.Get(ctx, actorID, id)
	if err != nil {
		return Node{}, err
	}
	// Fail before streaming: the right, the condition, the locks, the
	// quota — each checked again when committing.
	switch {
	case n.Kind != KindFile:
		return Node{}, ErrNotFile
	case !n.Perms.Has(access.Write):
		return Node{}, ErrForbidden
	case !cond.holds(n, true):
		return Node{}, ErrPrecondition
	}
	c, err := chain(ctx, s.db, id)
	if err != nil {
		return Node{}, err
	}
	if err := checkLocks(ctx, s.db, actorID, [][]chainEntry{c}, ""); err != nil {
		return Node{}, err
	}
	if size >= 0 {
		if err := s.fitsQuota(ctx, n.OwnerID, size); err != nil {
			return Node{}, err
		}
	}

	blobID := newUUID()
	st := staged{key: "uploads/fs-" + blobID, blobID: blobID}
	if s.store.Layout() != storage.LayoutTree {
		st.key = flatKey(blobID)
	}
	res, err := s.store.Put(ctx, st.key, r, size)
	if err != nil {
		if errors.Is(err, storage.ErrSizeMismatch) {
			return Node{}, ErrSizeMismatch
		}
		return Node{}, fmt.Errorf("store content: %w", err)
	}
	st.size, st.sha256 = res.Size, res.SHA256
	var written Node
	err = s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		chains, err := lockChains(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		node, err := getNode(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if err := allow(ctx, tx, actorID, &node, access.Write); err != nil {
			return nil, err
		}
		if node.Kind != KindFile {
			return nil, ErrNotFile
		}
		if !cond.holds(node, true) {
			return nil, ErrPrecondition
		}
		if err := checkLocks(ctx, tx, actorID, chains[:1], ""); err != nil {
			return nil, err
		}
		if err := checkQuota(ctx, tx, node.OwnerID, st.size); err != nil {
			return nil, err
		}
		written, undo, err = s.replaceContent(ctx, tx, actorID, node, chains[0][1:], st)
		written.Perms = node.Perms
		return undo, err
	})
	if err != nil {
		if delErr := s.store.Delete(context.WithoutCancel(ctx), st.key); delErr != nil {
			slog.ErrorContext(ctx, "fs: remove orphaned content", "key", st.key, "err", delErr)
		}
		return Node{}, err
	}
	return written, nil
}

// fitsQuota is checkQuota ahead of a write, outside its transaction: a
// cheap early answer, not a guarantee.
func (s *Service) fitsQuota(ctx context.Context, ownerID string, size int64) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = rollback(tx) }()
	return checkQuota(ctx, tx, ownerID, size)
}
