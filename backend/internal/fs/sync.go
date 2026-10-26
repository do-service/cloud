package fs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/do-service/cloud/backend/internal/access"
)

// What sync clients read (E11-2, docs/desktop-client.md): the change
// journal that the write paths fill (journal in repo.go), and whole
// subtrees to start from.

// SyncHead is where a user's tree journal stands (sync_heads); all zero
// for a tree nothing was journaled in yet.
type SyncHead struct {
	// TreeSeq is the last seq journaled in their tree.
	TreeSeq int64
	// MinSeq: rows up to it are deleted, a cursor older can't resume.
	MinSeq int64
	// AccessSeq changes when what they see of others' trees does.
	AccessSeq int64
}

// SyncView is what concerns a user's sync client, at one moment.
type SyncView struct {
	Own SyncHead
	// Shared are the nodes carrying a share to the user, by owner, with
	// the rights granted on each (access.Granted).
	Shared map[string]map[string]access.Perm
	// Heads are the heads of those owners' trees.
	Heads map[string]SyncHead
}

// Owners are the owners of what is shared with the user, sorted.
func (v SyncView) Owners() []string {
	return slices.Sorted(maps.Keys(v.Shared))
}

// syncTx is a read-only snapshot: everything read in it, the journal
// heads included, is of one moment.
var syncTx = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}

// SyncView returns userID's view, the heads and the shares at once.
func (s *Service) SyncView(ctx context.Context, userID string) (SyncView, error) {
	var view SyncView
	err := pgx.BeginTxFunc(ctx, s.db, syncTx, func(tx pgx.Tx) error {
		var err error
		view, err = syncView(ctx, tx, userID)
		return err
	})
	return view, err
}

func syncView(ctx context.Context, q querier, userID string) (SyncView, error) {
	shared, err := access.Granted(ctx, q, userID)
	if err != nil {
		return SyncView{}, fmt.Errorf("sync view: %w", err)
	}
	view := SyncView{Shared: shared, Heads: map[string]SyncHead{}}
	rows, err := q.Query(ctx, `
		SELECT user_id::text, tree_seq, min_seq, access_seq FROM sync_heads WHERE user_id = ANY($1::uuid[])`,
		append([]string{userID}, view.Owners()...))
	if err != nil {
		return SyncView{}, fmt.Errorf("sync view: %w", err)
	}
	var id string
	var h SyncHead
	_, err = pgx.ForEachRow(rows, []any{&id, &h.TreeSeq, &h.MinSeq, &h.AccessSeq}, func() error {
		if id == userID {
			view.Own = h
		} else {
			view.Heads[id] = h
		}
		return nil
	})
	if err != nil {
		return SyncView{}, fmt.Errorf("sync view: %w", err)
	}
	return view, nil
}

// String names the op as the sync API reports it.
func (o journalOp) String() string {
	switch o {
	case opCreate:
		return "create"
	case opContent:
		return "content"
	case opMove:
		return "move"
	case opTrash:
		return "trash"
	case opRestore:
		return "restore"
	}
	return fmt.Sprintf("op%d", int16(o))
}

// JournalEntry is a journal row: node NodeID changed at Seq.
type JournalEntry struct {
	Seq    int64
	NodeID string
	// Op is a hint at what happened: create, content, move (or rename),
	// trash, restore. The node's current state is what counts.
	Op string
}

// Journal returns up to limit rows of ownerID's tree journal after seq
// that concern actorID, as view (the actor's) has it: all rows of their
// own tree; of someone else's, those whose scopes hold a node shared with
// them. Rows stop at the tree's head in view, so all of them were
// committed when view was read (seqs follow commit order, see journal):
// a cursor that moves to the last row returned, or to the head when fewer
// came back, never skips one committed late. Whether a node is still
// visible is the caller's to check, with Get.
func (s *Service) Journal(ctx context.Context, actorID string, view SyncView, ownerID string, after int64, limit int) ([]JournalEntry, error) {
	head := view.Own
	var scopes []string
	if ownerID != actorID {
		shared, ok := view.Shared[ownerID]
		if !ok {
			return nil, nil
		}
		head = view.Heads[ownerID]
		scopes = slices.Collect(maps.Keys(shared))
	}
	if after >= head.TreeSeq || limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT seq, node_id::text, op FROM sync_journal
		WHERE owner_id = $1 AND seq > $2 AND seq <= $3 AND ($4::uuid[] IS NULL OR scopes && $4::uuid[])
		ORDER BY seq LIMIT $5`, ownerID, after, head.TreeSeq, scopes, limit)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	var out []JournalEntry
	var e JournalEntry
	var op journalOp
	_, err = pgx.ForEachRow(rows, []any{&e.Seq, &e.NodeID, &op}, func() error {
		e.Op = op.String()
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	return out, nil
}

// syncTreePage bounds the rows one query of SyncTree reads. Each page is
// read whole before any of it is passed on, so no statement waits on a
// slow client (and runs into statement_timeout) however big a folder is.
const syncTreePage = 5000

// SyncTree passes the live subtree of rootID as actorID sees it, with
// their rights on each node, all from one snapshot: first start gets the
// view at that moment and the root, then each gets the nodes below, a
// level at a time, so a folder always comes before its contents. A client
// that applies it and then the journal from that view misses nothing.
//
// Levels, as in updateSubtree: each query is an index range on
// (parent_id, name_key), where a recursive CTE's plan would rest on row
// estimates.
func (s *Service) SyncTree(ctx context.Context, actorID, rootID string, start func(SyncView, Node) error, each func(Node) error) error {
	if !validID(rootID) {
		return ErrNotFound
	}
	return pgx.BeginTxFunc(ctx, s.db, syncTx, func(tx pgx.Tx) error {
		root, err := getNode(ctx, tx, rootID)
		if err != nil {
			return err
		}
		if err := allow(ctx, tx, actorID, &root, access.Read); err != nil {
			return err
		}
		view, err := syncView(ctx, tx, actorID)
		if err != nil {
			return err
		}
		if err := start(view, root); err != nil {
			return err
		}
		if root.Kind != KindDir {
			return nil
		}
		// Rights below are the folder's plus what is shared right there:
		// shares are inherited down the tree (access.Perms). The owner's
		// are All already, with nothing granted.
		granted := view.Shared[root.OwnerID]
		perms := map[string]access.Perm{root.ID: root.Perms}
		for level := []string{root.ID}; len(level) > 0; {
			next := map[string]access.Perm{}
			var nextLevel []string
			for i := 0; i < len(level); i += syncTreePage {
				parents := level[i:min(i+syncTreePage, len(level))]
				err := treePages(ctx, tx, parents, func(n Node) error {
					n.Perms = perms[n.ParentID] | granted[n.ID]
					if n.Kind == KindDir {
						next[n.ID] = n.Perms
						nextLevel = append(nextLevel, n.ID)
					}
					return each(n)
				})
				if err != nil {
					return err
				}
			}
			level, perms = nextLevel, next
		}
		return nil
	})
}

// zeroUUID sorts before every node id: where treePages starts.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// treePages passes the live children of parents, a page at a time in
// (parent_id, name_key) order: keyset paging on the nodes_name_key index.
func treePages(ctx context.Context, tx pgx.Tx, parents []string, each func(Node) error) error {
	afterParent, afterKey := zeroUUID, ""
	for {
		rows, err := tx.Query(ctx, `
			SELECT `+nodeColumns+`, name_key FROM nodes
			WHERE parent_id = ANY($1::uuid[]) AND trashed_at IS NULL AND (parent_id, name_key) > ($2::uuid, $3)
			ORDER BY parent_id, name_key LIMIT $4`, parents, afterParent, afterKey, syncTreePage)
		if err != nil {
			return fmt.Errorf("sync tree: %w", err)
		}
		page := make([]Node, 0, syncTreePage)
		var key string
		for rows.Next() {
			n, err := scanNode(rows, &key)
			if err != nil {
				rows.Close()
				return fmt.Errorf("sync tree: %w", err)
			}
			page = append(page, n)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("sync tree: %w", err)
		}
		for _, n := range page {
			if err := each(n); err != nil {
				return err
			}
		}
		if len(page) < syncTreePage {
			return nil
		}
		afterParent, afterKey = page[len(page)-1].ParentID, key
	}
}

// TouchAccess moves on the access_seq of users and of the members of
// groups, in q's transaction: what they see of others' trees changed (a
// share to them made, changed or gone, a group joined or left), so their
// sync clients list the roots again, and a long poll of theirs wakes.
// Users no longer there are skipped. Rows are locked in id order, and the
// caller makes this its transaction's last statement: like journal's,
// these locks are taken last, so nothing waits while holding them.
func TouchAccess(ctx context.Context, q access.Querier, users, groups []string) error {
	if len(users) == 0 && len(groups) == 0 {
		return nil
	}
	_, err := q.Exec(ctx, `
		INSERT INTO sync_heads (user_id, access_seq)
		SELECT id, 1 FROM users
		WHERE id = ANY($1::uuid[]) OR id IN (SELECT user_id FROM group_members WHERE group_id = ANY($2::uuid[]))
		ORDER BY id
		ON CONFLICT (user_id) DO UPDATE SET access_seq = sync_heads.access_seq + 1`, users, groups)
	if err != nil {
		return fmt.Errorf("touch access: %w", err)
	}
	return nil
}

// SyncHeads returns the heads of users' trees: what a long poll compares
// against what its client has. A user nothing was journaled for has none.
func (s *Service) SyncHeads(ctx context.Context, users []string) (map[string]SyncHead, error) {
	rows, err := s.db.Query(ctx, `
		SELECT user_id::text, tree_seq, min_seq, access_seq FROM sync_heads WHERE user_id = ANY($1::uuid[])`, users)
	if err != nil {
		return nil, fmt.Errorf("sync heads: %w", err)
	}
	heads := map[string]SyncHead{}
	var id string
	var h SyncHead
	_, err = pgx.ForEachRow(rows, []any{&id, &h.TreeSeq, &h.MinSeq, &h.AccessSeq}, func() error {
		heads[id] = h
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sync heads: %w", err)
	}
	return heads, nil
}

// PruneJournal deletes up to limit journal rows written before cutoff
// and moves each tree's min_seq up to the last seq deleted from it, so a
// cursor from before then gets cursor_expired instead of a gap. Seqs
// follow commit order, not the rows' times (a transaction's now() is when
// it began), so a row below min_seq may outlive it: no cursor that can
// still resume reads it. Returns how many rows went.
func (s *Service) PruneJournal(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH gone AS (
				DELETE FROM sync_journal WHERE (owner_id, seq) IN (
					SELECT owner_id, seq FROM sync_journal WHERE at < $1 ORDER BY at LIMIT $2)
				RETURNING owner_id, seq
			)
			SELECT owner_id::text, max(seq), count(*) FROM gone GROUP BY owner_id ORDER BY owner_id`, cutoff, limit)
		if err != nil {
			return err
		}
		var owners []string
		var seqs []int64
		var owner string
		var seq, count int64
		_, err = pgx.ForEachRow(rows, []any{&owner, &seq, &count}, func() error {
			owners, seqs = append(owners, owner), append(seqs, seq)
			n += int(count)
			return nil
		})
		if err != nil || len(owners) == 0 {
			return err
		}
		// Heads locked in id order, as TouchAccess locks them: one
		// UPDATE's own order is the plan's, and two of them could each
		// hold a head the other waits for.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM sync_heads WHERE user_id = ANY($1::uuid[]) ORDER BY user_id FOR UPDATE`, owners); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE sync_heads h SET min_seq = g.seq FROM unnest($1::uuid[], $2::bigint[]) AS g(owner_id, seq)
			WHERE h.user_id = g.owner_id AND h.min_seq < g.seq`, owners, seqs)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("prune journal: %w", err)
	}
	return n, nil
}
