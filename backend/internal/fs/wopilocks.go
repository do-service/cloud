package fs

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/do-service/cloud/backend/internal/access"
)

// WOPI locks (E8-2) are the same locks, with the protocol's rules: the
// editor picks the lock string, which stands for the editing session of a
// document — however many users share it — so whoever presents it holds
// the lock. A conflict reports the current lock string, "" when the file
// is locked otherwise (by a WebDAV client) or not at all.

// WOPILockTimeout is the protocol's: editors refresh within it.
const WOPILockTimeout = 30 * time.Minute

// MaxWOPILock is the longest lock string the protocol allows.
const MaxWOPILock = 1024

// ErrBadLockID: an empty or too long WOPI lock string.
var ErrBadLockID = errors.New("fs: invalid WOPI lock")

// WOPILockToken is the lock token a WOPI lock string stands for on id, to
// present with WithLockTokens. Strings are the editor's, unique only per
// document, hence the id.
func WOPILockToken(id, lock string) string { return "wopi:" + id + ":" + lock }

// wopiLockOp is one of the protocol's lock operations, given the locks
// covering the file and what it is held by, if WOPI ("" otherwise).
type wopiLockOp func(tx pgx.Tx, covering []Lock, current string) error

// wopiLock runs op on file id for the actor, who needs write on it, and
// returns the lock string current before op ran.
func (s *Service) wopiLock(ctx context.Context, actorID, id string, op wopiLockOp) (string, error) {
	if !validID(id) {
		return "", ErrNotFound
	}
	var current string
	err := s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		chains, err := lockChains(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		n, err := getNode(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if err := allow(ctx, tx, actorID, &n, access.Write); err != nil {
			return nil, err
		}
		if n.Kind != KindFile {
			return nil, ErrNotFile
		}
		covering, err := coveringLocks(ctx, tx, chains[0])
		if err != nil {
			return nil, err
		}
		if len(covering) == 1 && covering[0].Kind == LockWOPI && covering[0].Above == 0 {
			current = strings.TrimPrefix(covering[0].Token, WOPILockToken(id, ""))
		}
		return nil, op(tx, covering, current)
	})
	return current, err
}

func validWOPILock(lock string) error {
	if lock == "" || len(lock) > MaxWOPILock {
		return ErrBadLockID
	}
	return nil
}

// LockWOPI takes lock on file id, or refreshes it if it is already the
// current one. With oldLock it is UnlockAndRelock: the file must be locked
// with oldLock, which lock replaces. On ErrLocked current is the lock in
// the way.
func (s *Service) LockWOPI(ctx context.Context, actorID, id, lock, oldLock string) (current string, err error) {
	if err := validWOPILock(lock); err != nil {
		return "", err
	}
	return s.wopiLock(ctx, actorID, id, func(tx pgx.Tx, covering []Lock, current string) error {
		switch {
		case oldLock == "" && len(covering) == 0:
			// An expired lock still holds the row.
			if _, err := tx.Exec(ctx, `DELETE FROM locks WHERE node_id = $1`, id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
				INSERT INTO locks (node_id, kind, token, owner_id, expires_at)
				VALUES ($1, 'wopi', $2, $3, now() + make_interval(secs => $4))`,
				id, WOPILockToken(id, lock), actorID, WOPILockTimeout.Seconds())
			return err
		case oldLock == "" && current == lock, oldLock != "" && current == oldLock:
			_, err := tx.Exec(ctx, `UPDATE locks SET token = $2, expires_at = now() + make_interval(secs => $3) WHERE node_id = $1`,
				id, WOPILockToken(id, lock), WOPILockTimeout.Seconds())
			return err
		}
		return ErrLocked
	})
}

// RefreshWOPILock extends lock on file id.
func (s *Service) RefreshWOPILock(ctx context.Context, actorID, id, lock string) (current string, err error) {
	return s.wopiLock(ctx, actorID, id, func(tx pgx.Tx, _ []Lock, current string) error {
		if current == "" || current != lock {
			return ErrLocked
		}
		_, err := tx.Exec(ctx, `UPDATE locks SET expires_at = now() + make_interval(secs => $2) WHERE node_id = $1`, id, WOPILockTimeout.Seconds())
		return err
	})
}

// UnlockWOPI removes lock from file id.
func (s *Service) UnlockWOPI(ctx context.Context, actorID, id, lock string) (current string, err error) {
	return s.wopiLock(ctx, actorID, id, func(tx pgx.Tx, _ []Lock, current string) error {
		if current == "" || current != lock {
			return ErrLocked
		}
		_, err := tx.Exec(ctx, `DELETE FROM locks WHERE node_id = $1`, id)
		return err
	})
}

// WOPILock returns the WOPI lock on file id ("" for none, or a lock that
// isn't WOPI's) and whether any lock covers it at all.
func (s *Service) WOPILock(ctx context.Context, actorID, id string) (current string, locked bool, err error) {
	current, err = s.wopiLock(ctx, actorID, id, func(_ pgx.Tx, covering []Lock, _ string) error {
		locked = len(covering) > 0
		return nil
	})
	return current, locked, err
}
