package fs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/do-service/cloud/backend/internal/storage"
)

// DeleteOwner removes everything userID owns — files, folders, versions,
// the trash, unfinished uploads, their root — and what was shared with
// them, for an account being deleted (E9-11). Each item goes through
// Trash and Purge, which keep blobs, versions and storage in step, one
// transaction per item. Safe to run again after a failure: it carries on
// with what is left.
func (s *Service) DeleteOwner(ctx context.Context, userID string) error {
	// Nobody keeps editing a deleted account's files, nor is held up by
	// a lock on them.
	if _, err := s.db.Exec(ctx, `DELETE FROM locks WHERE node_id IN (SELECT id FROM nodes WHERE owner_id = $1)`, userID); err != nil {
		return fmt.Errorf("delete owner: locks: %w", err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM shares WHERE grantee_type = 'user' AND grantee_id = $1`, userID); err != nil {
		return fmt.Errorf("delete owner: shares: %w", err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM uploads WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete owner: uploads: %w", err)
	}
	if err := s.EmptyTrash(ctx, userID); err != nil {
		return err
	}
	root, err := getRoot(ctx, s.db, userID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for {
		rows, err := s.db.Query(ctx, `SELECT id FROM nodes WHERE parent_id = $1 AND trashed_at IS NULL LIMIT 100`, root.ID)
		if err != nil {
			return fmt.Errorf("delete owner: list: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("delete owner: list: %w", err)
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if err := s.Trash(ctx, userID, id); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			if err := s.Purge(ctx, userID, id); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
	}
	// Anything trashed meanwhile.
	if err := s.EmptyTrash(ctx, userID); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM nodes WHERE id = $1`, root.ID); err != nil {
		return fmt.Errorf("delete owner: root: %w", err)
	}
	if s.store.Layout() == storage.LayoutTree {
		if err := s.store.Delete(ctx, userDirKey(userID)); err != nil {
			return fmt.Errorf("delete owner: user dir: %w", err)
		}
	}
	return nil
}
