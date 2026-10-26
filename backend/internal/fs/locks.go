package fs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/do-service/cloud/backend/internal/access"
)

// Write locks (R11, R12). A lock on a node keeps everyone from changing it
// — its content, name, place, properties, and for a folder what's in it —
// unless they hold the lock: are its owner and present its token, which a
// request carries in its context (WithLockTokens). Every write in this
// package checks, in its own transaction, so no protocol can skip it: a
// file a WebDAV client or an editor locked can't be replaced from the web
// UI either. A deep lock covers everything below its folder as well.

var (
	// ErrLocked: a lock the actor doesn't hold covers what they'd change.
	ErrLocked = errors.New("fs: locked")
	// ErrNoLock: no live lock has the token.
	ErrNoLock = errors.New("fs: no such lock")
)

// MaxLockTimeout bounds a lock's life: clients refresh what they keep, and
// a crashed one's lock doesn't block the file for long.
const MaxLockTimeout = time.Hour

type LockKind string

const (
	LockWebDAV LockKind = "webdav"
	LockWOPI   LockKind = "wopi"
)

// Lock is a held lock.
type Lock struct {
	NodeID  string
	Kind    LockKind
	Token   string
	OwnerID string
	Deep    bool
	// OwnerInfo is what the client said about the owner (WebDAV <owner>).
	OwnerInfo string
	ExpiresAt time.Time
	// Above is how many levels above the node it was asked for the lock
	// sits: 0 on the node itself, 1 on its folder, and so on.
	Above int
}

type lockTokensKey struct{}

// WithLockTokens marks the tokens a request presents: writes in ctx may
// change what those locks cover, if the actor owns them.
func WithLockTokens(ctx context.Context, tokens []string) context.Context {
	return context.WithValue(ctx, lockTokensKey{}, tokens)
}

func lockTokens(ctx context.Context) []string {
	tokens, _ := ctx.Value(lockTokensKey{}).([]string)
	return tokens
}

const lockColumns = `node_id::text, kind, token, owner_id::text, deep, owner_info, expires_at`

func scanLock(row pgx.Row, extra ...any) (Lock, error) {
	var l Lock
	err := row.Scan(append([]any{&l.NodeID, &l.Kind, &l.Token, &l.OwnerID, &l.Deep, &l.OwnerInfo, &l.ExpiresAt}, extra...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lock{}, ErrNoLock
	}
	return l, err
}

func collectLocks(rows pgx.Rows, err error) ([]Lock, error) {
	if err != nil {
		return nil, fmt.Errorf("locks: %w", err)
	}
	defer rows.Close()
	var locks []Lock
	for rows.Next() {
		var above int
		l, err := scanLock(rows, &above)
		if err != nil {
			return nil, fmt.Errorf("locks: %w", err)
		}
		l.Above = above
		locks = append(locks, l)
	}
	return locks, rows.Err()
}

// coveringLocks returns the live locks on c[0] and the deep ones on its
// ancestors, c being a chain from a node up to the root.
func coveringLocks(ctx context.Context, q querier, c []chainEntry) ([]Lock, error) {
	ids := make([]string, len(c))
	for i, e := range c {
		ids[i] = e.ID
	}
	// Above is the position in the chain.
	return collectLocks(q.Query(ctx, `
		SELECT `+lockColumns+`, array_position($1::uuid[], node_id) - 1 FROM locks
		WHERE expires_at > now() AND (node_id = ($1::uuid[])[1] OR deep AND node_id = ANY($1::uuid[]))`, ids))
}

// locksBelow returns the live locks on anything below dirID.
func locksBelow(ctx context.Context, q querier, dirID string) ([]Lock, error) {
	// From each lock up to the root, looking for the folder: locks are
	// few, a folder's subtree can be huge.
	return collectLocks(q.Query(ctx, `
		WITH RECURSIVE up AS (
			SELECT l.node_id AS lock_node, n.parent_id AS id FROM locks l JOIN nodes n ON n.id = l.node_id
			WHERE l.expires_at > now() AND n.trashed_at IS NULL
			UNION ALL
			SELECT up.lock_node, n.parent_id FROM up JOIN nodes n ON n.id = up.id WHERE up.id <> $1
		)
		SELECT `+lockColumns+`, -1 FROM locks WHERE node_id IN (SELECT lock_node FROM up WHERE id = $1)`, dirID))
}

// checkLocks fails with ErrLocked if a lock the actor doesn't hold covers
// any of chains (each from a node up to the root) or, for subtree, lies
// anywhere below that folder.
func checkLocks(ctx context.Context, q querier, actorID string, chains [][]chainEntry, subtree string) error {
	held := func(locks []Lock) error {
		tokens := lockTokens(ctx)
		for _, l := range locks {
			// A WOPI lock belongs to the document's editing session, which
			// several users can share: its string is the proof, not who
			// took it.
			if !slices.Contains(tokens, l.Token) || l.Kind != LockWOPI && l.OwnerID != actorID {
				return ErrLocked
			}
		}
		return nil
	}
	for _, c := range chains {
		locks, err := coveringLocks(ctx, q, c)
		if err != nil {
			return err
		}
		if err := held(locks); err != nil {
			return err
		}
	}
	if subtree == "" {
		return nil
	}
	locks, err := locksBelow(ctx, q, subtree)
	if err != nil {
		return err
	}
	return held(locks)
}

// LockRequest is a lock to take.
type LockRequest struct {
	Kind      LockKind
	Deep      bool
	OwnerInfo string
	// Timeout is capped at MaxLockTimeout; 0 or less means the cap.
	Timeout time.Duration
}

func lockTimeout(d time.Duration) time.Duration {
	if d <= 0 || d > MaxLockTimeout {
		return MaxLockTimeout
	}
	return d
}

// Lock locks id for the actor, who needs write on it. It fails with
// ErrLocked while any other lock covers the node, or for a deep lock lies
// below it: locks are exclusive.
func (s *Service) Lock(ctx context.Context, actorID, id string, req LockRequest) (Lock, error) {
	if !validID(id) {
		return Lock{}, ErrNotFound
	}
	var lock Lock
	err := s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		// The chain's row locks order this against writes and other
		// locks in the same part of the tree.
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
		existing, err := coveringLocks(ctx, tx, chains[0])
		if err != nil {
			return nil, err
		}
		if len(existing) > 0 {
			return nil, ErrLocked
		}
		if req.Deep && n.Kind == KindDir {
			below, err := locksBelow(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			if len(below) > 0 {
				return nil, ErrLocked
			}
		}
		// An expired lock still holds the row.
		if _, err := tx.Exec(ctx, `DELETE FROM locks WHERE node_id = $1`, id); err != nil {
			return nil, err
		}
		lock, err = scanLock(tx.QueryRow(ctx, `
			INSERT INTO locks (node_id, kind, token, owner_id, deep, owner_info, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7))
			RETURNING `+lockColumns,
			id, req.Kind, "opaquelocktoken:"+newUUID(), actorID, req.Deep && n.Kind == KindDir, req.OwnerInfo, lockTimeout(req.Timeout).Seconds()))
		return nil, err
	})
	return lock, err
}

// RefreshLock extends the actor's lock to timeout from now.
func (s *Service) RefreshLock(ctx context.Context, actorID, token string, timeout time.Duration) (Lock, error) {
	l, err := scanLock(s.db.QueryRow(ctx, `
		UPDATE locks SET expires_at = now() + make_interval(secs => $3)
		WHERE token = $1 AND owner_id = $2 AND expires_at > now() RETURNING `+lockColumns,
		token, actorID, lockTimeout(timeout).Seconds()))
	if errors.Is(err, ErrNoLock) {
		return Lock{}, s.foreignLock(ctx, token)
	}
	return l, err
}

// Unlock removes the actor's lock.
func (s *Service) Unlock(ctx context.Context, actorID, token string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM locks WHERE token = $1 AND owner_id = $2 AND expires_at > now()`, token, actorID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return s.foreignLock(ctx, token)
	}
	return nil
}

// foreignLock tells someone else's live lock (ErrForbidden) from none.
func (s *Service) foreignLock(ctx context.Context, token string) error {
	var exists bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM locks WHERE token = $1 AND expires_at > now())`, token).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrForbidden
	}
	return ErrNoLock
}

// Locks returns the live locks covering id and, with children, those
// covering each of its children, by node id: what a WebDAV lockdiscovery
// shows and an If header is checked against.
func (s *Service) Locks(ctx context.Context, actorID, id string, children bool) (map[string][]Lock, error) {
	n, err := s.Get(ctx, actorID, id)
	if err != nil {
		return nil, err
	}
	c, err := chain(ctx, s.db, n.ID)
	if err != nil {
		return nil, err
	}
	covering, err := coveringLocks(ctx, s.db, c)
	if err != nil {
		return nil, err
	}
	out := map[string][]Lock{}
	if len(covering) > 0 {
		out[n.ID] = covering
	}
	if !children {
		return out, nil
	}
	own, err := collectLocks(s.db.Query(ctx, `
		SELECT `+lockColumns+`, 0 FROM locks
		WHERE expires_at > now() AND node_id IN (SELECT id FROM nodes WHERE parent_id = $1 AND trashed_at IS NULL)`, n.ID))
	if err != nil {
		return nil, err
	}
	for _, l := range own {
		out[l.NodeID] = append(out[l.NodeID], l)
	}
	// The folder's deep locks cover each child, one level further up.
	var inherited []Lock
	for _, l := range covering {
		if l.Deep {
			l.Above++
			inherited = append(inherited, l)
		}
	}
	if len(inherited) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT id::text FROM nodes WHERE parent_id = $1 AND trashed_at IS NULL`, n.ID)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = append(out[id], inherited...)
	}
	return out, nil
}
