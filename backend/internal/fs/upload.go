package fs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/storage"
)

var (
	ErrQuotaExceeded    = errors.New("fs: not enough quota left for this upload")
	ErrChecksumMismatch = errors.New("fs: uploaded content doesn't match its checksum")
	ErrSizeMismatch     = errors.New("fs: uploaded content doesn't match its declared size")
)

// Conflict is what to do when a file's name is taken in its folder.
type Conflict string

const (
	ConflictFail Conflict = "fail"
	// ConflictRename picks "name (2).ext" and so on.
	ConflictRename Conflict = "rename"
	// ConflictReplace makes the content a new version of the file with the
	// name (R7); a folder there is still a conflict.
	ConflictReplace Conflict = "replace"
)

func (c Conflict) Valid() bool {
	return c == ConflictFail || c == ConflictRename || c == ConflictReplace
}

// UploadTTL is how long an unfinished upload holds its quota reservation.
const UploadTTL = 7 * 24 * time.Hour

// Upload is an upload in progress (R2): where its file will go, and the
// quota it holds until then (R16).
type Upload struct {
	ID       string
	UserID   string
	ParentID string
	Name     string
	Size     int64
	// SHA256 is the client's checksum, nil when it sent none.
	SHA256     []byte
	OnConflict Conflict
	// NodeID, when set, is the file whose content the upload replaces,
	// wherever the file is and whatever it's called by then: a sync
	// client's edit of a file it knows by id. ParentID and Name are then
	// the file's at reservation, OnConflict is ConflictReplace.
	NodeID string
	// IfMatch, with NodeID only, is the ETag the file must still have,
	// at reservation and at finish; "" requires nothing.
	IfMatch string
	// MTime is the file's modification time to keep; zero means the time
	// it is finished.
	MTime time.Time
}

// ReserveUpload checks that up can become a file (a folder the actor may
// write to, a valid name, free unless renaming is allowed) and reserves
// its size against the quota, returning it with a new ID. Checked again
// at FinishUpload: hours may pass in between.
func (s *Service) ReserveUpload(ctx context.Context, actorID string, up Upload) (Upload, error) {
	if up.NodeID != "" {
		return s.reserveReplace(ctx, actorID, up)
	}
	if up.IfMatch != "" {
		return Upload{}, fmt.Errorf("fs: malformed upload %+v: IfMatch needs NodeID", up)
	}
	if !validID(up.ParentID) {
		return Upload{}, ErrNotFound
	}
	name, key, err := NormalizeName(up.Name)
	if err != nil {
		return Upload{}, err
	}
	if up.Size < 0 || !up.OnConflict.Valid() || up.SHA256 != nil && len(up.SHA256) != sha256.Size {
		return Upload{}, fmt.Errorf("fs: malformed upload %+v", up)
	}
	up.ID, up.UserID, up.Name = newUUID(), actorID, name
	linkID, viaLink := access.LinkID(actorID)
	err = s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		parent, err := writableDir(ctx, tx, actorID, up.ParentID)
		if err != nil {
			return nil, err
		}
		// Refused now rather than after hours of uploading; checked again
		// when the upload finishes.
		c, err := chain(ctx, tx, parent.ID)
		if err != nil {
			return nil, err
		}
		if existing, err := liveChild(ctx, tx, parent.ID, key); err == nil && up.OnConflict == ConflictReplace {
			c = append([]chainEntry{{ID: existing.ID, Name: existing.Name}}, c...)
		}
		if err := checkLocks(ctx, tx, actorID, [][]chainEntry{c}, ""); err != nil {
			return nil, err
		}
		if up.OnConflict != ConflictRename {
			existing, err := liveChild(ctx, tx, parent.ID, key)
			switch {
			case errors.Is(err, ErrNotFound):
			case err != nil:
				return nil, err
			// Replacing works on a file only.
			case up.OnConflict == ConflictFail || existing.Kind != KindFile:
				return nil, ErrNameConflict
			}
		}
		if err := checkQuota(ctx, tx, parent.OwnerID, up.Size); err != nil {
			return nil, err
		}
		if viaLink {
			// Through a link: the owner's upload, continued only by the link.
			up.UserID = parent.OwnerID
		} else {
			linkID = ""
		}
		up.ParentID = parent.ID
		return nil, insertUpload(ctx, tx, up, linkID)
	})
	if err != nil {
		return Upload{}, err
	}
	return up, nil
}

// reserveReplace is ReserveUpload for an upload replacing file up.NodeID.
// Like WriteFile it needs write on the file alone, not on its folder.
func (s *Service) reserveReplace(ctx context.Context, actorID string, up Upload) (Upload, error) {
	if !validID(up.NodeID) {
		return Upload{}, ErrNotFound
	}
	if up.Size < 0 || up.SHA256 != nil && len(up.SHA256) != sha256.Size {
		return Upload{}, fmt.Errorf("fs: malformed upload %+v", up)
	}
	// An upload link's write is for adding files: it replaces nothing,
	// and may not learn what's in the folder.
	if _, viaLink := access.LinkID(actorID); viaLink {
		return Upload{}, ErrForbidden
	}
	up.ID, up.UserID, up.OnConflict = newUUID(), actorID, ConflictReplace
	err := s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		node, err := replaceable(ctx, tx, actorID, up)
		if err != nil {
			return nil, err
		}
		// Refused now rather than after hours of uploading; checked again
		// when the upload finishes.
		c, err := chain(ctx, tx, node.ID)
		if err != nil {
			return nil, err
		}
		if err := checkLocks(ctx, tx, actorID, [][]chainEntry{c}, ""); err != nil {
			return nil, err
		}
		if err := checkQuota(ctx, tx, node.OwnerID, up.Size); err != nil {
			return nil, err
		}
		up.ParentID, up.Name = node.ParentID, node.Name
		return nil, insertUpload(ctx, tx, up, "")
	})
	if err != nil {
		return Upload{}, err
	}
	return up, nil
}

// replaceable returns the file up.NodeID if actorID may write it and it
// still matches up.IfMatch.
func replaceable(ctx context.Context, q querier, actorID string, up Upload) (Node, error) {
	node, err := getNode(ctx, q, up.NodeID)
	if err != nil {
		return Node{}, err
	}
	if err := allow(ctx, q, actorID, &node, access.Write); err != nil {
		return Node{}, err
	}
	if node.Kind != KindFile {
		return Node{}, ErrNotFile
	}
	if up.IfMatch != "" && node.ETag != up.IfMatch {
		return Node{}, ErrPrecondition
	}
	return node, nil
}

func insertUpload(ctx context.Context, tx pgx.Tx, up Upload, linkID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO uploads (id, user_id, link_id, parent_id, name, size, sha256, on_conflict, node_id, if_match, mtime, expires_at)
		VALUES ($1, $2, nullif($3, '')::uuid, $4, $5, $6, $7, $8, nullif($9, '')::uuid, nullif($10, ''), $11,
			now() + make_interval(secs => $12))`,
		up.ID, up.UserID, linkID, up.ParentID, up.Name, up.Size, up.SHA256, up.OnConflict,
		up.NodeID, up.IfMatch, orNow(up.MTime), UploadTTL.Seconds())
	return err
}

// checkQuota fails with ErrQuotaExceeded unless size more bytes fit into
// the owner's quota, next to what is stored and what running uploads have
// reserved. The owner's row is locked till the transaction ends, which
// serializes the writes counting against one quota, so two can't both fit
// into the same free space. The sum is a separate statement to see
// reservations committed while this one waited.
func checkQuota(ctx context.Context, tx pgx.Tx, ownerID string, size int64) error {
	var quota *int64
	var used int64
	if err := tx.QueryRow(ctx, `SELECT quota_bytes, used_bytes FROM users WHERE id = $1 FOR UPDATE`, ownerID).Scan(&quota, &used); err != nil {
		return err
	}
	if quota == nil {
		return nil
	}
	var reserved int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(size), 0) FROM uploads WHERE user_id = $1 AND expires_at > now()`, ownerID).Scan(&reserved); err != nil {
		return err
	}
	if used+reserved+size > *quota {
		return ErrQuotaExceeded
	}
	return nil
}

// GetUpload returns the actor's upload id; anyone else's is ErrNotFound.
func (s *Service) GetUpload(ctx context.Context, actorID, id string) (Upload, error) {
	if !validID(id) {
		return Upload{}, ErrNotFound
	}
	who, arg, ok := uploadOwner(actorID)
	if !ok {
		return Upload{}, ErrNotFound
	}
	var up Upload
	var mtime *time.Time
	err := s.db.QueryRow(ctx, `
		SELECT id, user_id, parent_id, name, size, sha256, on_conflict, coalesce(node_id::text, ''), coalesce(if_match, ''), mtime
		FROM uploads WHERE id = $1 AND `+who, id, arg).
		Scan(&up.ID, &up.UserID, &up.ParentID, &up.Name, &up.Size, &up.SHA256, &up.OnConflict, &up.NodeID, &up.IfMatch, &mtime)
	if errors.Is(err, pgx.ErrNoRows) {
		return Upload{}, ErrNotFound
	}
	if mtime != nil {
		up.MTime = *mtime
	}
	return up, err
}

// CancelUpload drops the actor's upload id and its reservation; its data
// is the caller's to delete.
func (s *Service) CancelUpload(ctx context.Context, actorID, id string) error {
	if !validID(id) {
		return ErrNotFound
	}
	who, arg, ok := uploadOwner(actorID)
	if !ok {
		return ErrNotFound
	}
	_, err := s.db.Exec(ctx, `DELETE FROM uploads WHERE id = $1 AND `+who, id, arg)
	return err
}

// uploadOwner is the condition (on $2) picking the uploads actorID may
// continue: a user's own, or a link's. ok is false for a principal that
// can't own any.
func uploadOwner(actorID string) (cond, arg string, ok bool) {
	if linkID, viaLink := access.LinkID(actorID); viaLink {
		return "link_id = $2", linkID, validID(linkID)
	}
	return "user_id = $2 AND link_id IS NULL", actorID, validID(actorID)
}

// FinishUpload turns a complete upload, its content at stagingKey, into
// a file, and ends its reservation in the same transaction. The content is
// hashed here, reading it once more: the checksum the client sent is
// checked, and the node needs its own. On ErrChecksumMismatch or
// ErrSizeMismatch nothing changes; the caller discards the upload.
func (s *Service) FinishUpload(ctx context.Context, actorID, id, stagingKey string) (Node, error) {
	return s.finishUpload(ctx, actorID, id, stagingKey, nil)
}

// FinishUploadHashed is FinishUpload for content whose sha256 the caller
// computed while it was written, so it isn't read again: only its size is
// checked against the store, and sum against the client's checksum.
func (s *Service) FinishUploadHashed(ctx context.Context, actorID, id, stagingKey string, sum [32]byte) (Node, error) {
	return s.finishUpload(ctx, actorID, id, stagingKey, &sum)
}

// finishUpload hashes the content itself when sum is nil.
func (s *Service) finishUpload(ctx context.Context, actorID, id, stagingKey string, sum *[32]byte) (Node, error) {
	up, err := s.GetUpload(ctx, actorID, id)
	if err != nil {
		return Node{}, err
	}
	st := staged{key: stagingKey, blobID: newUUID(), mtime: up.MTime}
	if sum != nil {
		info, err := s.store.Stat(ctx, stagingKey)
		if err != nil {
			return Node{}, fmt.Errorf("stat upload: %w", err)
		}
		st.size, st.sha256 = info.Size, *sum
	} else if st.size, st.sha256, err = s.hashStaged(ctx, stagingKey); err != nil {
		return Node{}, err
	}
	if st.size != up.Size {
		return Node{}, ErrSizeMismatch
	}
	if up.SHA256 != nil && !bytes.Equal(up.SHA256, st.sha256[:]) {
		return Node{}, ErrChecksumMismatch
	}
	if s.store.Layout() != storage.LayoutTree {
		// A flat layout keeps content at its blob's own key.
		final := flatKey(st.blobID)
		if err := s.store.Move(ctx, stagingKey, final); err != nil {
			return Node{}, fmt.Errorf("store upload: %w", err)
		}
		st.key = final
	}
	endReservation := func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM uploads WHERE id = $1`, id)
		if err == nil && tag.RowsAffected() == 0 {
			// Finished or cancelled concurrently.
			return ErrNotFound
		}
		return err
	}
	if up.NodeID != "" {
		return s.finishReplace(ctx, actorID, up, st, endReservation)
	}
	return s.commitFile(ctx, actorID, up.ParentID, up.Name, st, up.OnConflict, endReservation)
}

// finishReplace makes st the content of file up.NodeID, as it is now: a
// rename or move since the reservation doesn't matter, the file is the
// same. up.IfMatch is checked here decisively, under the file's lock: the
// file may have changed while the upload ran. Whatever is left at the
// staging key on failure is removed, as by commitFile.
func (s *Service) finishReplace(ctx context.Context, actorID string, up Upload, st staged, also func(pgx.Tx) error) (Node, error) {
	var written Node
	err := s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		chains, err := lockChains(ctx, tx, up.NodeID)
		if err != nil {
			return nil, err
		}
		node, err := replaceable(ctx, tx, actorID, up)
		if err != nil {
			return nil, err
		}
		if err := checkLocks(ctx, tx, actorID, chains[:1], ""); err != nil {
			return nil, err
		}
		// The reservation already holds the quota.
		if err := also(tx); err != nil {
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

// hashStaged reads the content at key through, returning its size and sha256.
func (s *Service) hashStaged(ctx context.Context, key string) (size int64, sum [32]byte, err error) {
	rc, err := s.store.Get(ctx, key, nil)
	if err != nil {
		return 0, sum, fmt.Errorf("open upload: %w", err)
	}
	defer rc.Close()
	h := sha256.New()
	if size, err = io.Copy(h, rc); err != nil {
		return 0, sum, fmt.Errorf("hash upload: %w", err)
	}
	h.Sum(sum[:0])
	return size, sum, nil
}

// ExpiredUploads returns up to limit uploads whose reservation has lapsed,
// for the cleanup job to remove with their data.
func (s *Service) ExpiredUploads(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT id::text FROM uploads WHERE expires_at <= now() ORDER BY expires_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("expired uploads: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("expired uploads: %w", err)
	}
	return ids, nil
}

// UploadExists reports whether id is an upload in progress, expired or not.
func (s *Service) UploadExists(ctx context.Context, id string) (bool, error) {
	if !validID(id) {
		return false, nil
	}
	var exists bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM uploads WHERE id = $1)`, id).Scan(&exists)
	return exists, err
}

// DropUpload deletes upload id whoever it belongs to: the cleanup job's
// counterpart of CancelUpload. Its data is the caller's to delete.
func (s *Service) DropUpload(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM uploads WHERE id = $1`, id)
	return err
}
