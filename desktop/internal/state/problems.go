package state

import (
	"database/sql"
	"time"
)

// Problem is something sync can't resolve on its own: a name Windows
// can't store, a conflict copy, a full quota, an unsynced edit kept on
// unchecking a folder. Kind and Code are the engine's.
type Problem struct {
	ID        int64
	Kind      string
	RelPath   string
	NodeID    string
	Code      string
	Detail    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PutProblem records a problem, or refreshes Code, Detail and UpdatedAt of
// the one already recorded for the same (Kind, RelPath, NodeID), so a scan
// finding it again doesn't add another row.
func (t *Tx) PutProblem(p Problem) (int64, error) {
	now := time.Now().UnixMilli()
	var id int64
	err := t.queryRow(`
		INSERT INTO problems (kind, rel_path, node_id, code, detail, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind, rel_path, node_id) DO UPDATE SET code = excluded.code,
			detail = excluded.detail, updated_at = excluded.updated_at
		RETURNING id`,
		p.Kind, p.RelPath, p.NodeID, p.Code, p.Detail, now, now).Scan(&id)
	return id, err
}

func (t *Tx) Problems() ([]Problem, error) {
	rows, err := t.query(`SELECT id, kind, rel_path, node_id, code, detail, created_at, updated_at FROM problems ORDER BY id`)
	return collect(rows, err, func(r *sql.Rows) (Problem, error) {
		var p Problem
		var created, updated int64
		err := r.Scan(&p.ID, &p.Kind, &p.RelPath, &p.NodeID, &p.Code, &p.Detail, &created, &updated)
		p.CreatedAt, p.UpdatedAt = fromMillis(created), fromMillis(updated)
		return p, err
	})
}

func (t *Tx) DeleteProblem(id int64) error {
	_, err := t.exec(`DELETE FROM problems WHERE id = ?`, id)
	return err
}

// ResolveProblems deletes the problems of a kind at a path, when what
// caused them is gone.
func (t *Tx) ResolveProblems(kind, relPath string) error {
	_, err := t.exec(`DELETE FROM problems WHERE kind = ? AND rel_path = ?`, kind, relPath)
	return err
}

// ActivityLimit is how many activity entries are kept.
const ActivityLimit = 1000

// Activity is an entry of «Активность»: a file uploaded, downloaded,
// deleted, a conflict copy made. Kind is the engine's.
type Activity struct {
	ID      int64
	At      time.Time
	Kind    string
	RelPath string
	Detail  string
}

// AddActivity appends an entry and drops the ones beyond ActivityLimit.
func (t *Tx) AddActivity(a Activity) error {
	at := a.At
	if at.IsZero() {
		at = time.Now()
	}
	res, err := t.exec(`INSERT INTO activity (at, kind, rel_path, detail) VALUES (?, ?, ?, ?)`, at.UnixMilli(), a.Kind, a.RelPath, a.Detail)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	// AUTOINCREMENT ids never repeat, so the window is by id.
	_, err = t.exec(`DELETE FROM activity WHERE id <= ?`, id-ActivityLimit)
	return err
}

// RecentActivity lists up to limit entries, newest first.
func (t *Tx) RecentActivity(limit int) ([]Activity, error) {
	rows, err := t.query(`SELECT id, at, kind, rel_path, detail FROM activity ORDER BY id DESC LIMIT ?`, limit)
	return collect(rows, err, func(r *sql.Rows) (Activity, error) {
		var a Activity
		var at int64
		err := r.Scan(&a.ID, &at, &a.Kind, &a.RelPath, &a.Detail)
		a.At = fromMillis(at)
		return a, err
	})
}
