package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// OpState of a journaled op.
type OpState string

const (
	// OpPending: the intent is recorded, the action not started.
	OpPending OpState = "pending"
	// OpRunning: the action started and may or may not have happened —
	// what an op left in this state after a crash means.
	OpRunning OpState = "running"
	// OpFailed: the last attempt failed with ErrorCode; retried at NextTry,
	// or never by itself when NextTry is zero (waiting for the user).
	OpFailed OpState = "failed"
)

// Op is one intended action of the engine. Kind and Params are the
// engine's: this package stores them without interpreting.
type Op struct {
	ID     int64
	Kind   string
	NodeID string
	// Path is slash-separated, relative to the sync folder.
	Path string
	// Params is JSON; nil is stored as {}.
	Params    json.RawMessage
	State     OpState
	Attempts  int
	NextTry   time.Time
	ErrorCode string
	CreatedAt time.Time
}

const opCols = `id, kind, node_id, path, params, state, attempts, next_try, error_code, created_at`

func scanOp(r interface{ Scan(...any) error }) (Op, error) {
	var o Op
	var params string
	var next, created int64
	err := r.Scan(&o.ID, &o.Kind, &o.NodeID, &o.Path, &params, &o.State, &o.Attempts, &next, &o.ErrorCode, &created)
	o.Params = json.RawMessage(params)
	o.NextTry = fromMillis(next)
	o.CreatedAt = fromMillis(created)
	return o, err
}

func scanOpRows(r *sql.Rows) (Op, error) { return scanOp(r) }

// AddOp records an intent and returns its id. Commit it before the action
// runs: an action without a recorded intent is what a crash can't recover.
func (t *Tx) AddOp(o Op) (int64, error) {
	params := string(o.Params)
	if params == "" {
		params = "{}"
	}
	res, err := t.exec(`INSERT INTO ops (kind, node_id, path, params, state, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		o.Kind, o.NodeID, o.Path, params, OpPending, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (t *Tx) Op(id int64) (Op, error) {
	o, err := scanOp(t.queryRow(`SELECT `+opCols+` FROM ops WHERE id = ?`, id))
	return o, notFound(err)
}

// Ops lists every op in the journal in the order they were recorded: on
// start, all of them are unfinished business, running ones first to be
// checked against what actually happened.
func (t *Tx) Ops() ([]Op, error) {
	rows, err := t.query(`SELECT ` + opCols + ` FROM ops ORDER BY id`)
	return collect(rows, err, scanOpRows)
}

// DueOps lists pending ops and failed ones whose retry time has come.
func (t *Tx) DueOps(now time.Time, limit int) ([]Op, error) {
	rows, err := t.query(`
		SELECT `+opCols+` FROM ops
		WHERE state = 'pending' OR (state = 'failed' AND next_try > 0 AND next_try <= ?)
		ORDER BY id LIMIT ?`, now.UnixMilli(), limit)
	return collect(rows, err, scanOpRows)
}

// StartOp marks the action as begun and counts the attempt.
func (t *Tx) StartOp(id int64) error {
	return t.updateOp(`UPDATE ops SET state = 'running', attempts = attempts + 1 WHERE id = ?`, id)
}

// FailOp records a failed attempt; a zero nextTry leaves the op for the
// user or the engine to resolve.
func (t *Tx) FailOp(id int64, code string, nextTry time.Time) error {
	return t.updateOp(`UPDATE ops SET state = 'failed', error_code = ?, next_try = ? WHERE id = ?`, code, toMillis(nextTry), id)
}

// RetryOp makes an op pending again (after a restart found it running, or
// when the user asks).
func (t *Tx) RetryOp(id int64) error {
	return t.updateOp(`UPDATE ops SET state = 'pending', next_try = 0 WHERE id = ?`, id)
}

// DeleteOp ends an op; its transfer row goes with it.
func (t *Tx) DeleteOp(id int64) error {
	_, err := t.exec(`DELETE FROM ops WHERE id = ?`, id)
	return err
}

func (t *Tx) updateOp(query string, args ...any) error {
	res, err := t.exec(query, args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return ErrNotFound
	}
	return nil
}

// Intend records the intent of an action in its own transaction, the
// first step of the journal pattern:
//
//	id, _ := db.Intend(ctx, op)         // intent
//	… the action (idempotent) …
//	db.Complete(ctx, id, func(tx) { tx.PutBase(…) }) // result + op gone, one tx
//
// After a crash the op is still in Ops(), and redoing it is safe.
func (d *DB) Intend(ctx context.Context, o Op) (int64, error) {
	var id int64
	err := d.Update(ctx, func(tx *Tx) error {
		var err error
		id, err = tx.AddOp(o)
		return err
	})
	return id, err
}

// IntendRunning is Intend and StartOp in one transaction: for an action
// that starts right away, one commit (one fsync) less. After a crash the
// op is running, as it would be after StartOp.
func (d *DB) IntendRunning(ctx context.Context, o Op) (int64, error) {
	var id int64
	err := d.Update(ctx, func(tx *Tx) error {
		var err error
		if id, err = tx.AddOp(o); err != nil {
			return err
		}
		return tx.StartOp(id)
	})
	return id, err
}

// Complete records the result of op id with apply and deletes the op, in
// one transaction: either both happen or neither. ErrNotFound when the op
// is gone (completed elsewhere), and then apply's writes are rolled
// back too.
func (d *DB) Complete(ctx context.Context, id int64, apply func(*Tx) error) error {
	return d.Update(ctx, func(tx *Tx) error {
		if apply != nil {
			if err := apply(tx); err != nil {
				return err
			}
		}
		res, err := tx.exec(`DELETE FROM ops WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// Transfer is the resumable part of an upload or download op.
type Transfer struct {
	OpID int64
	// TusURL is the upload's location, to resume with HEAD + PATCH.
	TusURL string
	// Done is how many bytes the server (upload) or the .part file
	// (download) holds.
	Done int64
	// PartPath is the download's .part file, absolute.
	PartPath string
}

func (t *Tx) PutTransfer(x Transfer) error {
	_, err := t.exec(`
		INSERT INTO transfers (op_id, tus_url, done, part_path) VALUES (?, ?, ?, ?)
		ON CONFLICT (op_id) DO UPDATE SET tus_url = excluded.tus_url, done = excluded.done, part_path = excluded.part_path`,
		x.OpID, x.TusURL, x.Done, x.PartPath)
	return err
}

func (t *Tx) Transfer(opID int64) (Transfer, error) {
	var x Transfer
	err := t.queryRow(`SELECT op_id, tus_url, done, part_path FROM transfers WHERE op_id = ?`, opID).
		Scan(&x.OpID, &x.TusURL, &x.Done, &x.PartPath)
	return x, notFound(err)
}

func toMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
