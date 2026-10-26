package fs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// querier is what both the pool and a transaction offer.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const nodeColumns = `id, owner_id, parent_id, name, kind, size, coalesce(mime, ''), etag, mtime, content_hash, blob_id`

// scanNode reads nodeColumns, then any extra columns into extra.
func scanNode(row pgx.Row, extra ...any) (Node, error) {
	var n Node
	var parentID, blobID *string
	err := row.Scan(append([]any{&n.ID, &n.OwnerID, &parentID, &n.Name, &n.Kind, &n.Size, &n.MIME, &n.ETag, &n.MTime, &n.ContentHash, &blobID}, extra...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, err
	}
	if parentID != nil {
		n.ParentID = *parentID
	}
	if blobID != nil {
		n.blobID = *blobID
	}
	return n, nil
}

// getNode returns a live (not trashed) node.
func getNode(ctx context.Context, q querier, id string) (Node, error) {
	return scanNode(q.QueryRow(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = $1 AND trashed_at IS NULL`, id))
}

func getRoot(ctx context.Context, q querier, ownerID string) (Node, error) {
	return scanNode(q.QueryRow(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE owner_id = $1 AND parent_id IS NULL`, ownerID))
}

func insertRoot(ctx context.Context, q querier, ownerID string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO nodes (owner_id, name, name_key, kind) VALUES ($1, '', '', 'dir')
		ON CONFLICT (owner_id) WHERE parent_id IS NULL DO NOTHING`, ownerID)
	return err
}

type chainEntry struct {
	ID   string
	Name string
}

// chain returns id and its ancestors, from id up to the root. Empty when
// id doesn't exist.
func chain(ctx context.Context, q querier, id string) ([]chainEntry, error) {
	rows, err := q.Query(ctx, `
		WITH RECURSIVE anc AS (
			SELECT id, parent_id, name, 0 AS depth FROM nodes WHERE id = $1
			UNION ALL
			SELECT n.id, n.parent_id, n.name, a.depth + 1 FROM nodes n JOIN anc a ON n.id = a.parent_id
		)
		SELECT id, name FROM anc ORDER BY depth`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[chainEntry])
}

// lockNodes row-locks ids in id order. Every operation takes its locks
// through here, so two of them never wait on each other in opposite order.
func lockNodes(ctx context.Context, tx pgx.Tx, ids []string) error {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	_, err := tx.Exec(ctx, `SELECT 1 FROM nodes WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`, sorted)
	return err
}

type newNode struct {
	ownerID, parentID, name, key string
	kind                         Kind
	size                         int64
	mime                         string
	blobID                       string
	contentHash                  []byte
	// modifiedBy wrote a file's content; empty for a folder.
	modifiedBy string
	// mtime zero means now.
	mtime time.Time
}

func insertNode(ctx context.Context, tx pgx.Tx, n newNode) (Node, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO nodes (owner_id, parent_id, name, name_key, kind, size, mime, blob_id, content_hash, modified_by, mtime)
		VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, '')::uuid, $9, nullif($10, '')::uuid, coalesce($11, now()))
		RETURNING `+nodeColumns,
		n.ownerID, n.parentID, n.name, n.key, n.kind, n.size, n.mime, n.blobID, n.contentHash, n.modifiedBy, orNow(n.mtime))
	node, err := scanNode(row)
	return node, mapConflict(err)
}

// orNow is t as a query argument that coalesce(…, now()) turns into the
// transaction's time when t is zero.
func orNow(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func updatePlacement(ctx context.Context, tx pgx.Tx, id, parentID, name, key string) (Node, error) {
	row := tx.QueryRow(ctx, `
		UPDATE nodes SET parent_id = $2, name = $3, name_key = $4, updated_at = now()
		WHERE id = $1 RETURNING `+nodeColumns, id, parentID, name, key)
	node, err := scanNode(row)
	return node, mapConflict(err)
}

// bumpAncestors adds delta to the size of dirID and every ancestor and
// gives them all a new ETag, so sync clients see the change from the root
// down. Only dirID itself gets a new mtime: like a POSIX directory, a
// folder's mtime reflects changes to its direct children.
func bumpAncestors(ctx context.Context, tx pgx.Tx, dirID string, delta int64) error {
	_, err := tx.Exec(ctx, `
		WITH RECURSIVE anc AS (
			SELECT id, parent_id FROM nodes WHERE id = $1
			UNION ALL
			SELECT n.id, n.parent_id FROM nodes n JOIN anc a ON n.id = a.parent_id
		)
		UPDATE nodes SET
			size = nodes.size + $2,
			etag = replace(gen_random_uuid()::text, '-', ''),
			mtime = CASE WHEN nodes.id = $1 THEN now() ELSE nodes.mtime END,
			updated_at = now()
		FROM anc WHERE nodes.id = anc.id`, dirID, delta)
	return err
}

// journalOp says what happened to a node in a sync_journal row. It is a
// hint: the sync API reports a node's current state, not the operation.
type journalOp int16

const (
	opCreate journalOp = iota + 1
	opContent
	opMove // rename too
	opTrash
	opRestore
)

type deviceKey struct{}

// WithDevice marks the device a request comes from: changes made in ctx
// are journaled as its own, so its sync client can tell them apart.
func WithDevice(ctx context.Context, deviceID string) context.Context {
	return context.WithValue(ctx, deviceKey{}, deviceID)
}

func deviceOf(ctx context.Context) string {
	id, _ := ctx.Value(deviceKey{}).(string)
	return id
}

// journal records in sync_journal that nodeID in ownerID's tree changed by
// op. chains are the node's chains the operation locked, old and new
// (each from a node up to the root): the shared nodes on them, and nodeID
// itself if shared, become the row's scopes. Every write that creates a
// node or changes its place, name, content or trash state calls it in its
// transaction, as the last statement before the storage step. Not
// journaled: ancestors' sizes and ETags (they follow from the change),
// nodes below a moved or trashed folder, and purges of trashed nodes.
//
// The seq comes from the owner's sync_heads row, whose lock the increment
// holds until commit, so the next write to the tree gets its seq only once
// this one has committed: seqs are handed out in commit order, and a
// reader that sees seq N sees everything below it too — a cursor never
// skips a row committed late. The row is locked last, after the node
// locks: every write to a tree has locked that tree's root already
// (lockChains goes up to it), so writers queue on the root, not here, and
// one transaction only ever journals one owner — no lock cycle through
// sync_heads.
func journal(ctx context.Context, tx pgx.Tx, ownerID, nodeID string, op journalOp, chains ...[]chainEntry) error {
	ids := []string{nodeID}
	for _, c := range chains {
		for _, e := range c {
			ids = append(ids, e.ID)
		}
	}
	_, err := tx.Exec(ctx, `
		WITH head AS (
			INSERT INTO sync_heads (user_id, tree_seq) VALUES ($1, 1)
			ON CONFLICT (user_id) DO UPDATE SET tree_seq = sync_heads.tree_seq + 1
			RETURNING tree_seq
		)
		INSERT INTO sync_journal (owner_id, seq, node_id, op, scopes, device_id)
		SELECT $1, head.tree_seq, $2, $3,
			(SELECT array_agg(DISTINCT node_id) FROM shares WHERE node_id = ANY($4::uuid[])),
			nullif($5, '')::uuid
		FROM head`, ownerID, nodeID, op, ids, deviceOf(ctx))
	if err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	return nil
}

func insertBlob(ctx context.Context, tx pgx.Tx, id, backend, storageKey string, size int64, hash []byte) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO blobs (id, backend, storage_key, size, hash) VALUES ($1, $2, nullif($3, ''), $4, $5)`,
		id, backend, storageKey, size, hash)
	return err
}

func addUsedBytes(ctx context.Context, tx pgx.Tx, userID string, delta int64) error {
	tag, err := tx.Exec(ctx, `UPDATE users SET used_bytes = used_bytes + $2 WHERE id = $1`, userID, delta)
	if err == nil && tag.RowsAffected() != 1 {
		return fmt.Errorf("update used bytes of user %s: %w", userID, ErrNotFound)
	}
	return err
}

// mapConflict turns the unique-name violation into ErrNameConflict.
func mapConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "nodes_name_key" {
		return ErrNameConflict
	}
	return err
}

// pathLockKey maps a node to its advisory lock. Path locks are advisory,
// not row locks, because every read takes them: FOR SHARE would write to
// the tuple of the user's root on each download, advisory locks live only
// in memory. A hash collision only makes two unrelated nodes wait on each
// other.
const pathLockKey = `hashtextextended(%s::text, 0)`

// lockPathExclusive keeps readers from resolving any path through id until
// the transaction ends: taken by the operations that change paths.
func lockPathExclusive(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(`+fmt.Sprintf(pathLockKey, "$1")+`)`, id)
	return err
}

// lockPathShared shares-locks id and its ancestors and returns the chain as
// it is once they are locked. Like lockChains it re-reads the chain until
// every node on it is locked: a statement sees the data as of its start,
// so the chain read before a wait for a mover can be stale.
func lockPathShared(ctx context.Context, tx pgx.Tx, id string) ([]chainEntry, error) {
	locked := map[string]bool{}
	for range maxLockRounds {
		c, err := chain(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if len(c) == 0 {
			return nil, ErrNotFound
		}
		var missing []string
		for _, e := range c {
			if !locked[e.ID] {
				missing = append(missing, e.ID)
			}
		}
		if len(missing) == 0 {
			return c, nil
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(`+fmt.Sprintf(pathLockKey, "id")+`) FROM unnest($1::uuid[]) AS id`, missing); err != nil {
			return nil, err
		}
		for _, id := range missing {
			locked[id] = true
		}
	}
	return nil, errChainUnstable
}

// sortColumns are the columns List orders by; each has an index on
// (parent_id, kind, column, id).
var sortColumns = map[Sort]string{SortName: "name_key", SortMTime: "mtime", SortSize: "size"}

// listChildren returns up to limit live children of kind under parentID
// in sort order, after the cursor position from if given, with the
// name_key of each for the next cursor.
func listChildren(ctx context.Context, q querier, parentID string, kind Kind, sort Sort, desc bool, from *cursor, limit int) ([]Node, []string, error) {
	col := sortColumns[sort]
	dir, cmp := "ASC", ">"
	if desc {
		dir, cmp = "DESC", "<"
	}
	args := []any{parentID, kind, limit}
	where := ""
	if from != nil {
		v, err := sortValue(sort, from.Value)
		if err != nil {
			return nil, nil, ErrInvalidCursor
		}
		// A row comparison matches the index order, so the scan starts
		// at the cursor instead of filtering up to it.
		where = fmt.Sprintf(` AND (%s, id) %s ($4, $5)`, col, cmp)
		args = append(args, v, from.ID)
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`
		SELECT `+nodeColumns+`, name_key FROM nodes
		WHERE parent_id = $1 AND kind = $2 AND trashed_at IS NULL%s
		ORDER BY %s %s, id %s LIMIT $3`, where, col, dir, dir), args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list children: %w", err)
	}
	defer rows.Close()
	var nodes []Node
	var keys []string
	for rows.Next() {
		var key string
		n, err := scanNode(rows, &key)
		if err != nil {
			return nil, nil, fmt.Errorf("list children: %w", err)
		}
		nodes, keys = append(nodes, n), append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list children: %w", err)
	}
	return nodes, keys, nil
}

// walk returns the live subtree below dirID. Paths sort bytewise, which
// puts every folder before its contents.
func walk(ctx context.Context, q querier, dirID string) ([]Entry, error) {
	rows, err := q.Query(ctx, `
		WITH RECURSIVE t AS (
			SELECT id, kind, name AS path, size, mtime FROM nodes
			WHERE parent_id = $1 AND trashed_at IS NULL
			UNION ALL
			SELECT n.id, n.kind, t.path || '/' || n.name, n.size, n.mtime FROM nodes n JOIN t ON n.parent_id = t.id
			WHERE t.kind = 'dir' AND n.trashed_at IS NULL
		)
		SELECT id, path, kind, size, mtime FROM t ORDER BY path COLLATE "C"`, dirID)
	if err != nil {
		return nil, fmt.Errorf("walk: %w", err)
	}
	entries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Entry])
	if err != nil {
		return nil, fmt.Errorf("walk: %w", err)
	}
	return entries, nil
}

// markTrashed stamps id and every live node below it with one trashed_at
// (now() is the transaction's start time), flagging id as the trash item.
// Nodes trashed earlier keep their stamp.
func markTrashed(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, `UPDATE nodes SET trashed_at = now(), trash_root = true, updated_at = now() WHERE id = $1`, id); err != nil {
		return err
	}
	return updateSubtree(ctx, tx, id, `UPDATE nodes SET trashed_at = now(), updated_at = now()
		WHERE parent_id = ANY($1::uuid[]) AND trashed_at IS NULL RETURNING id::text, kind`)
}

// unmarkTrashed undoes markTrashed for id: the nodes deleted with it (the
// same trashed_at) come back, those deleted on their own before stay in
// the trash.
func unmarkTrashed(ctx context.Context, tx pgx.Tx, id string) error {
	var at time.Time
	if err := tx.QueryRow(ctx, `SELECT trashed_at FROM nodes WHERE id = $1`, id).Scan(&at); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE nodes SET trashed_at = NULL, trash_root = false, updated_at = now() WHERE id = $1`, id); err != nil {
		return mapConflict(err)
	}
	return updateSubtree(ctx, tx, id, `UPDATE nodes SET trashed_at = NULL, updated_at = now()
		WHERE parent_id = ANY($1::uuid[]) AND trashed_at = $2 AND NOT trash_root RETURNING id::text, kind`, at)
}

// updateSubtree runs update, which must take the parent ids as $1 and
// return (id, kind) of the rows it changed, on the children of id, then
// of the folders among those, and so on down. A level at a time keeps
// every step an index lookup on parent_id. One recursive CTE would be a
// single statement, but its plan rests on row estimates that lag behind
// mass changes like these, and a nested loop over the work table it then
// picks took minutes on a 50 000-node folder.
func updateSubtree(ctx context.Context, tx pgx.Tx, id, update string, args ...any) error {
	for level := []string{id}; len(level) > 0; {
		rows, err := tx.Query(ctx, update, append([]any{level}, args...)...)
		if err != nil {
			return err
		}
		var next []string
		var childID string
		var kind Kind
		_, err = pgx.ForEachRow(rows, []any{&childID, &kind}, func() error {
			if kind == KindDir {
				next = append(next, childID)
			}
			return nil
		})
		if err != nil {
			return mapConflict(err)
		}
		level = next
	}
	return nil
}

// lockTrashRoot row-locks the trash item id of actorID, so a restore and
// a purge of it can't run at once.
func lockTrashRoot(ctx context.Context, tx pgx.Tx, actorID, id string) (Node, error) {
	n, err := scanNode(tx.QueryRow(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = $1 AND trash_root FOR UPDATE`, id))
	if err != nil {
		return Node{}, err
	}
	return n, ownerOnly(actorID, n)
}

// parentPath is a select-list expression for the path of a nodes row's
// folder from the root, "Docs/2026", "" for the root: one walk up the
// primary key per row, a few levels deep.
const parentPath = `coalesce((
	WITH RECURSIVE anc AS (
		SELECT p.parent_id, p.name, 0 AS depth FROM nodes p WHERE p.id = nodes.parent_id
		UNION ALL
		SELECT x.parent_id, x.name, a.depth + 1 FROM nodes x JOIN anc a ON x.id = a.parent_id
	)
	SELECT string_agg(name, '/' ORDER BY depth DESC) FROM anc WHERE name <> ''
), '')`

// listTrash returns up to limit of ownerID's trash items after the cursor,
// newest first, each with the path of the folder it was in.
func listTrash(ctx context.Context, q querier, ownerID string, after *trashCursor, limit int) ([]TrashItem, error) {
	args := []any{ownerID, limit}
	where := ""
	if after != nil {
		where = ` AND (trashed_at, id) < ($3, $4)`
		args = append(args, after.At, after.ID)
	}
	rows, err := q.Query(ctx, `
		SELECT `+nodeColumns+`, trashed_at, `+parentPath+`
		FROM nodes WHERE owner_id = $1 AND trash_root`+where+`
		ORDER BY trashed_at DESC, id DESC LIMIT $2`, args...)
	if err != nil {
		return nil, fmt.Errorf("list trash: %w", err)
	}
	defer rows.Close()
	var items []TrashItem
	for rows.Next() {
		var it TrashItem
		if it.Node, err = scanNode(rows, &it.TrashedAt, &it.OriginalPath); err != nil {
			return nil, fmt.Errorf("list trash: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list trash: %w", err)
	}
	return items, nil
}

// releaseBlobs drops one reference per occurrence of an id in ids and
// deletes the blobs nothing references anymore, returning the storage keys
// of those that have one (versions, flat layouts).
func releaseBlobs(ctx context.Context, tx pgx.Tx, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	const refs = `(SELECT id, count(*) AS n FROM unnest($1::uuid[]) AS id GROUP BY id) r`
	rows, err := tx.Query(ctx, `DELETE FROM blobs b USING `+refs+` WHERE b.id = r.id AND b.refcount <= r.n RETURNING b.id::text, b.storage_key`, ids)
	if err != nil {
		return nil, fmt.Errorf("release blobs: %w", err)
	}
	type gone struct {
		ID  string
		Key *string
	}
	deleted, err := pgx.CollectRows(rows, pgx.RowToStructByPos[gone])
	if err != nil {
		return nil, fmt.Errorf("release blobs: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE blobs b SET refcount = b.refcount - r.n FROM `+refs+` WHERE b.id = r.id`, ids); err != nil {
		return nil, fmt.Errorf("release blobs: %w", err)
	}
	var keys []string
	for _, g := range deleted {
		if g.Key != nil {
			keys = append(keys, *g.Key)
		}
		// Its previews go with it (their rows by the foreign key).
		keys = append(keys, PreviewKey(g.ID, ""))
	}
	return keys, nil
}

// searchNodes returns up to limit of ownerID's live nodes whose name_key
// matches the LIKE pattern, after the cursor, by name_key then id, with
// each one's folder path and name_key (for the next cursor). The owner and
// pattern together are one nodes_search index scan.
func searchNodes(ctx context.Context, q querier, ownerID, pattern string, after *searchCursor, limit int) ([]SearchResult, []string, error) {
	args := []any{ownerID, pattern, limit}
	where := ""
	if after != nil {
		where = ` AND (name_key, id) > ($4, $5)`
		args = append(args, after.Key, after.ID)
	}
	rows, err := q.Query(ctx, `
		SELECT `+nodeColumns+`, name_key, `+parentPath+`
		FROM nodes
		WHERE owner_id = $1 AND trashed_at IS NULL AND name_key LIKE $2`+where+`
		ORDER BY name_key, id LIMIT $3`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var items []SearchResult
	var keys []string
	for rows.Next() {
		var it SearchResult
		var key string
		if it.Node, err = scanNode(rows, &key, &it.Path); err != nil {
			return nil, nil, err
		}
		items, keys = append(items, it), append(keys, key)
	}
	return items, keys, rows.Err()
}

// liveChild returns parentID's live child with the name key.
func liveChild(ctx context.Context, q querier, parentID, key string) (Node, error) {
	return scanNode(q.QueryRow(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE parent_id = $1 AND name_key = $2 AND trashed_at IS NULL`, parentID, key))
}
