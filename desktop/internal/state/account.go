package state

import (
	"database/sql"
	"path/filepath"
	"strings"
)

// Account is the one account a database syncs.
type Account struct {
	Server   string
	UserID   string
	DeviceID string
	// Cursor of /sync/changes; "" until the first full listing.
	Cursor string
	// LocalRoot is the sync folder, absolute.
	LocalRoot string
	// WatchSince is the last watcher event id handled.
	WatchSince uint64
}

// Account returns ErrNotFound before SetAccount.
func (t *Tx) Account() (Account, error) {
	var a Account
	var since int64
	err := t.queryRow(`SELECT server, user_id, device_id, cursor, local_root, watch_since FROM account WHERE id = 1`).
		Scan(&a.Server, &a.UserID, &a.DeviceID, &a.Cursor, &a.LocalRoot, &since)
	// FSEvents ids are uint64 and SQLite integers int64: stored as the
	// same 64 bits.
	a.WatchSince = uint64(since)
	return a, notFound(err)
}

func (t *Tx) SetAccount(a Account) error {
	_, err := t.exec(`
		INSERT INTO account (id, server, user_id, device_id, cursor, local_root, watch_since)
		VALUES (1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET server = excluded.server, user_id = excluded.user_id,
			device_id = excluded.device_id, cursor = excluded.cursor,
			local_root = excluded.local_root, watch_since = excluded.watch_since`,
		a.Server, a.UserID, a.DeviceID, a.Cursor, a.LocalRoot, int64(a.WatchSince))
	return err
}

// SetCursor saves the changes cursor; commit it in the transaction that
// applies the changes it covers, or a crash would skip them.
func (t *Tx) SetCursor(cursor string) error {
	return t.updateAccount(`UPDATE account SET cursor = ? WHERE id = 1`, cursor)
}

func (t *Tx) SetWatchSince(id uint64) error {
	return t.updateAccount(`UPDATE account SET watch_since = ? WHERE id = 1`, int64(id))
}

// Paused reports whether the user paused sync.
func (t *Tx) Paused() (bool, error) {
	var p int64
	err := t.queryRow(`SELECT paused FROM account WHERE id = 1`).Scan(&p)
	return p != 0, notFound(err)
}

func (t *Tx) SetPaused(paused bool) error {
	v := 0
	if paused {
		v = 1
	}
	return t.updateAccount(`UPDATE account SET paused = ? WHERE id = 1`, v)
}

// MoveRoot records that the sync folder moved to root: the account's
// folder and the absolute .part paths of resumable downloads, which
// would otherwise point into the old place and be lost. Everything else
// is relative to the folder. Idempotent: done again after a crash, it
// finds nothing left to change.
func (t *Tx) MoveRoot(root string) error {
	a, err := t.Account()
	if err != nil {
		return err
	}
	old := a.LocalRoot
	if old == root {
		return nil
	}
	rows, err := t.query(`SELECT op_id, part_path FROM transfers WHERE part_path != ''`)
	if err != nil {
		return err
	}
	moved := map[int64]string{}
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return err
		}
		if rel, err := filepath.Rel(old, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			moved[id] = filepath.Join(root, rel)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for id, p := range moved {
		if _, err := t.exec(`UPDATE transfers SET part_path = ? WHERE op_id = ?`, p, id); err != nil {
			return err
		}
	}
	return t.updateAccount(`UPDATE account SET local_root = ? WHERE id = 1`, root)
}

func (t *Tx) updateAccount(query string, arg any) error {
	res, err := t.exec(query, arg)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return ErrNotFound
	}
	return nil
}

// Perm is the caller's rights on a node, as the server's permission
// names ("read", "write", "reshare").
type Perm uint8

const (
	PermRead Perm = 1 << iota
	PermWrite
	PermReshare
)

func (p Perm) Has(bit Perm) bool { return p&bit != 0 }

// ParsePerms maps the server's names; unknown ones are ignored, so a newer
// server adding a right doesn't break an older client.
func ParsePerms(names []string) Perm {
	var p Perm
	for _, n := range names {
		switch n {
		case "read":
			p |= PermRead
		case "write":
			p |= PermWrite
		case "reshare":
			p |= PermReshare
		}
	}
	return p
}

// RootKind is "own" for the user's root, "shared" for one shared with them.
type RootKind string

const (
	RootOwn    RootKind = "own"
	RootShared RootKind = "shared"
)

// Root is an entry of /sync/roots.
type Root struct {
	NodeID    string
	Kind      RootKind
	Name      string
	OwnerID   string
	OwnerName string
	Perms     Perm
	ETag      string
}

func (t *Tx) Roots() ([]Root, error) {
	rows, err := t.query(`SELECT node_id, kind, name, owner_id, owner_name, perms, etag FROM roots ORDER BY kind, name`)
	return collect(rows, err, func(r *sql.Rows) (Root, error) {
		var x Root
		err := r.Scan(&x.NodeID, &x.Kind, &x.Name, &x.OwnerID, &x.OwnerName, &x.Perms, &x.ETag)
		return x, err
	})
}

// ReplaceRoots stores a fresh /sync/roots listing.
func (t *Tx) ReplaceRoots(roots []Root) error {
	if _, err := t.exec(`DELETE FROM roots`); err != nil {
		return err
	}
	for _, r := range roots {
		if _, err := t.exec(`INSERT INTO roots (node_id, kind, name, owner_id, owner_name, perms, etag) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.NodeID, r.Kind, r.Name, r.OwnerID, r.OwnerName, r.Perms, r.ETag); err != nil {
			return err
		}
	}
	return nil
}

// RuleMode of selective sync.
type RuleMode string

const (
	Include RuleMode = "include"
	Exclude RuleMode = "exclude"
)

// Rules maps node id to its rule; the engine resolves the nearest one up a
// chain in memory.
func (t *Tx) Rules() (map[string]RuleMode, error) {
	rows, err := t.query(`SELECT node_id, mode FROM rules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]RuleMode{}
	for rows.Next() {
		var id string
		var mode RuleMode
		if err := rows.Scan(&id, &mode); err != nil {
			return nil, err
		}
		m[id] = mode
	}
	return m, rows.Err()
}

func (t *Tx) SetRule(nodeID string, mode RuleMode) error {
	_, err := t.exec(`INSERT INTO rules (node_id, mode) VALUES (?, ?) ON CONFLICT (node_id) DO UPDATE SET mode = excluded.mode`, nodeID, mode)
	return err
}

// DeleteRule makes the node inherit its parent's rule again.
func (t *Tx) DeleteRule(nodeID string) error {
	_, err := t.exec(`DELETE FROM rules WHERE node_id = ?`, nodeID)
	return err
}
