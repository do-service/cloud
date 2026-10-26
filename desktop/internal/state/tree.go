package state

import (
	"database/sql"
)

// Kind of a node, named as the server names it.
type Kind string

const (
	KindDir  Kind = "dir"
	KindFile Kind = "file"
)

// Remote is a node of the server's tree as the client last heard of it.
type Remote struct {
	NodeID string
	// ParentID is "" at the top of a root.
	ParentID string
	Name     string
	// NameKey is the server's key of Name (localfs.NameKey).
	NameKey string
	Kind    Kind
	Size    int64
	ETag    string
	// SHA256 is hex, "" for folders.
	SHA256  string
	MtimeNS int64
	Perms   Perm
}

const remoteCols = `node_id, parent_id, name, name_key, kind, size, etag, sha256, mtime_ns, perms`

func scanRemote(r interface{ Scan(...any) error }) (Remote, error) {
	var x Remote
	var parent sql.NullString
	err := r.Scan(&x.NodeID, &parent, &x.Name, &x.NameKey, &x.Kind, &x.Size, &x.ETag, &x.SHA256, &x.MtimeNS, &x.Perms)
	x.ParentID = parent.String
	return x, err
}

func scanRemoteRows(r *sql.Rows) (Remote, error) { return scanRemote(r) }

func (t *Tx) PutRemote(n Remote) error {
	_, err := t.exec(`
		INSERT INTO remote (`+remoteCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (node_id) DO UPDATE SET parent_id = excluded.parent_id, name = excluded.name,
			name_key = excluded.name_key, kind = excluded.kind, size = excluded.size, etag = excluded.etag,
			sha256 = excluded.sha256, mtime_ns = excluded.mtime_ns, perms = excluded.perms`,
		n.NodeID, sql.NullString{String: n.ParentID, Valid: n.ParentID != ""}, n.Name, n.NameKey, n.Kind,
		n.Size, n.ETag, n.SHA256, n.MtimeNS, n.Perms)
	return err
}

// DeleteRemote forgets one node; its children, if any, are the caller's
// to delete (the server's tombstones name each).
func (t *Tx) DeleteRemote(nodeID string) error {
	_, err := t.exec(`DELETE FROM remote WHERE node_id = ?`, nodeID)
	return err
}

// ClearRemote empties the mirror before a full resync (410 cursor_expired).
func (t *Tx) ClearRemote() error {
	_, err := t.exec(`DELETE FROM remote`)
	return err
}

func (t *Tx) Remote(nodeID string) (Remote, error) {
	n, err := scanRemote(t.queryRow(`SELECT `+remoteCols+` FROM remote WHERE node_id = ?`, nodeID))
	return n, notFound(err)
}

func (t *Tx) RemoteChildren(parentID string) ([]Remote, error) {
	rows, err := t.query(`SELECT `+remoteCols+` FROM remote WHERE parent_id = ? ORDER BY name_key`, parentID)
	return collect(rows, err, scanRemoteRows)
}

// RemoteChild finds a child by the server's name key.
func (t *Tx) RemoteChild(parentID, nameKey string) (Remote, error) {
	n, err := scanRemote(t.queryRow(`SELECT `+remoteCols+` FROM remote WHERE parent_id = ? AND name_key = ?`, parentID, nameKey))
	return n, notFound(err)
}

// EachRemote calls fn for every node, unordered.
func (t *Tx) EachRemote(fn func(Remote) error) error {
	rows, err := t.query(`SELECT ` + remoteCols + ` FROM remote`)
	return each(rows, err, scanRemoteRows, fn)
}

// Local is a file or folder of the last scan.
type Local struct {
	// RelPath is slash-separated, relative to the sync folder, with names
	// as the file system returned them (on macOS possibly NFD).
	RelPath string
	Kind    Kind
	Size    int64
	MtimeNS int64
	// FileID is localfs.FileID.String().
	FileID string
	// SHA256Cached is the hash of the content at this size, mtime and
	// file id; "" when not computed.
	SHA256Cached string
}

const localCols = `rel_path, kind, size, mtime_ns, file_id, sha256_cached`

func scanLocal(r interface{ Scan(...any) error }) (Local, error) {
	var x Local
	err := r.Scan(&x.RelPath, &x.Kind, &x.Size, &x.MtimeNS, &x.FileID, &x.SHA256Cached)
	return x, err
}

func scanLocalRows(r *sql.Rows) (Local, error) { return scanLocal(r) }

func (t *Tx) PutLocal(l Local) error {
	_, err := t.exec(`
		INSERT INTO local (`+localCols+`) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (rel_path) DO UPDATE SET kind = excluded.kind, size = excluded.size,
			mtime_ns = excluded.mtime_ns, file_id = excluded.file_id, sha256_cached = excluded.sha256_cached`,
		l.RelPath, l.Kind, l.Size, l.MtimeNS, l.FileID, l.SHA256Cached)
	return err
}

func (t *Tx) Local(relPath string) (Local, error) {
	l, err := scanLocal(t.queryRow(`SELECT `+localCols+` FROM local WHERE rel_path = ?`, relPath))
	return l, notFound(err)
}

func (t *Tx) LocalByFileID(fileID string) ([]Local, error) {
	rows, err := t.query(`SELECT `+localCols+` FROM local WHERE file_id = ?`, fileID)
	return collect(rows, err, scanLocalRows)
}

// DeleteLocal forgets relPath and everything under it, as a partial scan
// of a subtree replaces what the previous scan saw there.
func (t *Tx) DeleteLocal(relPath string) error {
	if relPath == "" {
		_, err := t.exec(`DELETE FROM local`)
		return err
	}
	_, err := t.exec(`DELETE FROM local WHERE rel_path = ?1 OR substr(rel_path, 1, length(?1) + 1) = ?1 || '/'`, relPath)
	return err
}

// MoveLocal re-paths relPath and everything under it, as a rename the
// engine made itself moves what the last scan saw there; a row already
// at the destination is replaced.
func (t *Tx) MoveLocal(from, to string) error {
	if _, err := t.exec(`DELETE FROM local WHERE rel_path = ?1 OR substr(rel_path, 1, length(?1) + 1) = ?1 || '/'`, to); err != nil {
		return err
	}
	_, err := t.exec(`
		UPDATE local SET rel_path = ?2 || substr(rel_path, length(?1) + 1)
		WHERE rel_path = ?1 OR substr(rel_path, 1, length(?1) + 1) = ?1 || '/'`, from, to)
	return err
}

func (t *Tx) EachLocal(fn func(Local) error) error {
	rows, err := t.query(`SELECT ` + localCols + ` FROM local`)
	return each(rows, err, scanLocalRows, fn)
}

// Base is what local and remote last agreed on for a node.
type Base struct {
	NodeID  string
	RelPath string
	Kind    Kind
	SHA256  string
	ETag    string
	FileID  string
	Size    int64
	// MtimeNS is the local mtime as stat returned it after the sync, which
	// may be coarser than the server's (FAT: 2 s, HFS+: 1 s).
	MtimeNS int64
}

const baseCols = `node_id, rel_path, kind, sha256, etag, file_id, size, mtime_ns`

func scanBase(r interface{ Scan(...any) error }) (Base, error) {
	var x Base
	err := r.Scan(&x.NodeID, &x.RelPath, &x.Kind, &x.SHA256, &x.ETag, &x.FileID, &x.Size, &x.MtimeNS)
	return x, err
}

func scanBaseRows(r *sql.Rows) (Base, error) { return scanBase(r) }

// PutBase upserts by node id. RelPath is unique: moving a node onto the
// path of another fails until that one is moved or deleted first, in the
// same transaction.
func (t *Tx) PutBase(b Base) error {
	_, err := t.exec(`
		INSERT INTO base (`+baseCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (node_id) DO UPDATE SET rel_path = excluded.rel_path, kind = excluded.kind,
			sha256 = excluded.sha256, etag = excluded.etag, file_id = excluded.file_id,
			size = excluded.size, mtime_ns = excluded.mtime_ns`,
		b.NodeID, b.RelPath, b.Kind, b.SHA256, b.ETag, b.FileID, b.Size, b.MtimeNS)
	return err
}

func (t *Tx) Base(nodeID string) (Base, error) {
	b, err := scanBase(t.queryRow(`SELECT `+baseCols+` FROM base WHERE node_id = ?`, nodeID))
	return b, notFound(err)
}

func (t *Tx) BaseByPath(relPath string) (Base, error) {
	b, err := scanBase(t.queryRow(`SELECT `+baseCols+` FROM base WHERE rel_path = ?`, relPath))
	return b, notFound(err)
}

func (t *Tx) BaseByFileID(fileID string) ([]Base, error) {
	rows, err := t.query(`SELECT `+baseCols+` FROM base WHERE file_id = ?`, fileID)
	return collect(rows, err, scanBaseRows)
}

// DeleteBase forgets one node.
func (t *Tx) DeleteBase(nodeID string) error {
	_, err := t.exec(`DELETE FROM base WHERE node_id = ?`, nodeID)
	return err
}

// BaseUnder lists the rows at relPath and below it.
func (t *Tx) BaseUnder(relPath string) ([]Base, error) {
	rows, err := t.query(`SELECT `+baseCols+` FROM base WHERE rel_path = ?1 OR substr(rel_path, 1, length(?1) + 1) = ?1 || '/'`, relPath)
	return collect(rows, err, scanBaseRows)
}

// DeleteBaseUnder forgets relPath and everything under it: a folder
// trashed on the server takes its subtree, which the server doesn't
// journal node by node.
func (t *Tx) DeleteBaseUnder(relPath string) error {
	_, err := t.exec(`DELETE FROM base WHERE rel_path = ?1 OR substr(rel_path, 1, length(?1) + 1) = ?1 || '/'`, relPath)
	return err
}

// MoveBase re-paths a node and everything under it: a moved folder
// carries its subtree, and the server doesn't journal the children.
func (t *Tx) MoveBase(from, to string) error {
	_, err := t.exec(`
		UPDATE base SET rel_path = ?2 || substr(rel_path, length(?1) + 1)
		WHERE rel_path = ?1 OR substr(rel_path, 1, length(?1) + 1) = ?1 || '/'`, from, to)
	return err
}

func (t *Tx) EachBase(fn func(Base) error) error {
	rows, err := t.query(`SELECT ` + baseCols + ` FROM base`)
	return each(rows, err, scanBaseRows, fn)
}
