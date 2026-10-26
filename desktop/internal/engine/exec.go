package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net"
	"net/url"
	"sync"
	"syscall"
	"time"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// The executor runs a plan's ops through the journal: the intent is
// committed, the action runs, and its result goes into base (and the
// mirror and local table) in the transaction that deletes the op. An op
// left over by a crash is run again on the next start: every action
// checks first whether it already happened.

// errReplan: the world isn't what the op was planned on (a precondition
// failed, a name was taken, a file changed); the next pass plans again.
var errReplan = errors.New("engine: state changed since the plan")

// execute runs ops phase by phase; within a phase, structure ops one by
// one in plan order (parents first), deletes and transfers in parallel.
// Large transfers go to the background (control.go) with outer, the
// context of Pass's caller; an op on a path one of them holds waits for
// a later pass.
func (e *Engine) execute(ctx, outer context.Context, ops []Op, res *PassResult) error {
	for phase := 0; phase <= 3; phase++ {
		var batch []Op
		for _, o := range ops {
			if o.Kind.phase() != phase {
				continue
			}
			if e.bgBlocks(o) {
				res.Deferred++
				continue
			}
			if phase == 3 && e.background(o) {
				if err := e.runBackground(ctx, outer, o, res); err != nil {
					return err
				}
				continue
			}
			batch = append(batch, o)
		}
		if len(batch) == 0 {
			continue
		}
		var err error
		switch phase {
		case 0, 1:
			for _, o := range batch {
				if err = e.runNew(ctx, o, res); err != nil {
					break
				}
			}
		default:
			s := newSema(e.cfg.SmallTransfers)
			err = e.parallel(ctx, batch, res, func(Op) *sema { return s })
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// runBackground journals a large transfer and starts it in the
// background; with every large slot taken it waits for a later pass.
// The journal row comes first, so the next pass sees the node busy.
func (e *Engine) runBackground(ctx, outer context.Context, o Op, res *PassResult) error {
	if e.bgRunning() >= e.cfg.LargeTransfers {
		res.Deferred++
		return nil
	}
	params, err := json.Marshal(o)
	if err != nil {
		return err
	}
	id, err := e.db.IntendRunning(ctx, state.Op{Kind: string(o.Kind), NodeID: o.NodeID, Path: o.Path, Params: params})
	if err != nil {
		return err
	}
	if !e.startBackground(outer, id, o, true) {
		// Paused meanwhile, or the slot went: the op runs from the journal.
		res.Deferred++
		return e.db.Update(context.WithoutCancel(ctx), func(tx *state.Tx) error { return tx.RetryOp(id) })
	}
	return nil
}

type sema chan struct{}

func newSema(n int) *sema { s := make(sema, n); return &s }

// parallel runs ops with at most the semaphore's width at once; locks
// keep ops on overlapping paths apart. The first fatal error stops the
// rest.
func (e *Engine) parallel(ctx context.Context, ops []Op, res *PassResult, slot func(Op) *sema) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		locks = newPathLocks()
	)
	for _, o := range ops {
		s := slot(o)
		select {
		case *s <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(o Op) {
			defer wg.Done()
			var once sync.Once
			release := func() { once.Do(func() { <-*s }) }
			defer release()
			unlock := locks.lock(o.Path)
			defer unlock()
			var r PassResult
			// The slot may be freed early (releaseSlot). Not with one
			// slot: the property test keeps ops strictly one after
			// another, so a seed replays the same run.
			octx := ctx
			if cap(*s) > 1 {
				octx = context.WithValue(ctx, slotKey{}, release)
			}
			err := e.runNew(octx, o, &r)
			mu.Lock()
			res.Ops += r.Ops
			res.Failed += r.Failed
			if err != nil && first == nil {
				first = err
				cancel()
			}
			mu.Unlock()
		}(o)
	}
	wg.Wait()
	if first == nil && ctx.Err() != nil {
		first = ctx.Err()
	}
	return first
}

// slotKey carries a parallel op's release of its transfer slot.
type slotKey struct{}

// releaseSlot frees the op's transfer slot early, when its network part
// is over: the next transfer starts while this one waits for its flushes
// (durable.go), which then cover more files at once.
func releaseSlot(ctx context.Context) {
	if release, ok := ctx.Value(slotKey{}).(func()); ok {
		release()
	}
}

// runNew journals a planned op and runs it. Base-only ops have no
// action, so no journal either: their result is one transaction.
func (e *Engine) runNew(ctx context.Context, o Op, res *PassResult) error {
	if !o.Kind.journaled() {
		res.Ops++
		if err := e.db.Update(ctx, func(tx *state.Tx) error { return e.applyBaseOp(tx, o) }); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			res.Failed++
			b := e.noteFailure(o, err)
			e.log.Warn("sync: base update failed", "op", o.Kind, "path", o.Path, "err", err)
			e.recordProblem(ctx, o, err, b)
		}
		return nil
	}
	params, err := json.Marshal(o)
	if err != nil {
		return err
	}
	id, err := e.db.IntendRunning(ctx, state.Op{Kind: string(o.Kind), NodeID: o.NodeID, Path: o.Path, Params: params})
	if err != nil {
		return err
	}
	return e.run(ctx, id, o, res, true)
}

// run does one journaled op: action, then result and op deletion in one
// transaction. Only a fatal error (signed out, the test hook) is
// returned; others are recorded on the op's key and the plan moves on.
func (e *Engine) run(ctx context.Context, id int64, o Op, res *PassResult, started bool) error {
	res.Ops++
	if !started {
		if err := e.db.Update(ctx, func(tx *state.Tx) error { return tx.StartOp(id) }); err != nil {
			return err
		}
	}
	if e.cfg.Hook != nil {
		if err := e.cfg.Hook("act", o); err != nil {
			return err
		}
	}
	apply, err := e.act(ctx, id, o)
	if e.cfg.Hook != nil {
		if herr := e.cfg.Hook("acted", o); herr != nil {
			return herr
		}
	}
	releaseSlot(ctx)
	if err == nil {
		// The action happened: its result is recorded even if ctx ends
		// now (a pause), or the op would wait for a restart.
		ctx := context.WithoutCancel(ctx)
		if o.Kind.local() {
			if serr := e.sync.Wait(); serr != nil {
				res.Failed++
				e.log.Error("sync: flushing local changes failed", "op", o.Kind, "path", o.Path, "err", serr)
				return serr
			}
		}
		e.pullMu.Lock()
		cerr := e.db.Complete(ctx, id, apply)
		e.mirrorGen++
		e.pullMu.Unlock()
		if cerr != nil {
			res.Failed++
			e.log.Error("sync: recording an op's result failed", "op", o.Kind, "path", o.Path, "err", cerr)
			return cerr
		}
		e.succeeded(o)
		e.log.Info("sync: "+string(o.Kind), "path", o.Path)
		return nil
	}
	res.Failed++
	if errors.Is(err, api.ErrUnauthorized) {
		return ErrNeedsSignIn
	}
	if errors.Is(err, errStop) {
		return err
	}
	if ctx.Err() != nil {
		// Paused or stopped: the op is due again (a resumable transfer
		// carries on from its offset) instead of waiting for a restart.
		if rerr := e.db.Update(context.WithoutCancel(ctx), func(tx *state.Tx) error { return tx.RetryOp(id) }); rerr != nil {
			e.log.Warn("sync: requeueing a cut-off op failed", "op", o.Kind, "path", o.Path, "err", rerr)
		}
		return ctx.Err()
	}
	return e.failed(ctx, id, o, apply, err)
}

// failed sorts a failure out: transfers cut off by the network keep their
// op (and its resumable transfer) for a retry; the rest are dropped and
// planned again once their backoff ends. Problems that need the user are
// recorded.
func (e *Engine) failed(ctx context.Context, id int64, o Op, apply func(*state.Tx) error, err error) error {
	b := e.noteFailure(o, err)
	keep := (o.Kind == OpUpload || o.Kind == OpDownload) && (transient(err) || errors.Is(err, api.ErrLocked))
	level := e.log.Info
	if !errors.Is(err, errReplan) && !errors.Is(err, api.ErrPrecondition) && !errors.Is(err, localfs.ErrChanged) {
		level = e.log.Warn
	}
	level("sync: "+string(o.Kind)+" failed", "path", o.Path, "err", err, "retry", time.Until(b.until).Round(time.Second))
	if apply != nil && !keep && o.Kind.local() {
		if serr := e.sync.Wait(); serr != nil {
			return serr
		}
	}
	dbErr := e.db.Update(ctx, func(tx *state.Tx) error {
		if keep {
			return tx.FailOp(id, errorCode(err), b.until)
		}
		if apply != nil {
			// Part of it happened (some files of a remove); the flush of
			// those went before this transaction (below).
			if aerr := apply(tx); aerr != nil {
				return aerr
			}
		}
		return tx.DeleteOp(id)
	})
	if dbErr != nil {
		return dbErr
	}
	e.recordProblem(ctx, o, err, b)
	return nil
}

func (e *Engine) recordProblem(ctx context.Context, o Op, err error, b *backoff) {
	var p *state.Problem
	switch {
	case errors.Is(err, api.ErrQuota):
		p = &state.Problem{Kind: ProblemQuota, RelPath: o.Path, NodeID: o.NodeID, Detail: "not enough space in the cloud"}
	case errors.Is(err, api.ErrForbidden):
		p = &state.Problem{Kind: ProblemForbidden, RelPath: o.Path, NodeID: o.NodeID, Detail: err.Error()}
	case errors.Is(err, api.ErrLocked) && time.Since(b.first) >= e.cfg.LockRetryFor:
		p = &state.Problem{Kind: ProblemLocked, RelPath: o.Path, NodeID: o.NodeID, Detail: "locked on the server"}
	case errors.Is(err, localfs.ErrNotEmpty):
		p = &state.Problem{Kind: ProblemUnselectKept, RelPath: o.Path, NodeID: o.NodeID, Detail: "the folder holds files this client doesn't sync"}
	case b.attempts >= 5 && !errors.Is(err, api.ErrLocked):
		p = &state.Problem{Kind: ProblemFailing, RelPath: o.Path, NodeID: o.NodeID, Code: string(o.Kind), Detail: err.Error()}
	}
	if p == nil {
		return
	}
	_ = e.db.Update(context.WithoutCancel(ctx), func(tx *state.Tx) error {
		_, err := tx.PutProblem(*p)
		return err
	})
}

func errorCode(err error) string {
	var ae *api.APIError
	switch {
	case errors.As(err, &ae) && ae.Code != "":
		return ae.Code
	case errors.As(err, &ae):
		return fmt.Sprint(ae.Status)
	case transient(err):
		return "network"
	}
	return "error"
}

// transient: the network or the server's trouble, not the op's.
func transient(err error) bool {
	var ae *api.APIError
	if errors.As(err, &ae) {
		return ae.Status >= 500 && ae.Status != 507 || ae.Status == 429
	}
	var ne net.Error
	var ue *url.Error
	return errors.As(err, &ne) || errors.As(err, &ue) || errors.Is(err, api.ErrTruncated)
}

// backoff of one op key (Op.key): while it lasts, the planner leaves the
// node or path alone.
type backoff struct {
	attempts int
	first    time.Time
	until    time.Time
}

func (e *Engine) noteFailure(o Op, err error) *backoff {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := e.backoff[o.key()]
	if b == nil {
		b = &backoff{first: time.Now()}
		e.backoff[o.key()] = b
	}
	b.attempts++
	var d time.Duration
	switch {
	case errors.Is(err, errReplan), errors.Is(err, api.ErrPrecondition), errors.Is(err, localfs.ErrChanged),
		errors.Is(err, api.ErrNameConflict), errors.Is(err, localfs.ErrExists):
		// The next pass usually knows better; repeats back off.
		d = time.Duration(b.attempts-1) * time.Second
	case errors.Is(err, api.ErrLocked):
		d = e.cfg.LockBackoff
		if time.Since(b.first) >= e.cfg.LockRetryFor {
			d = 10 * time.Minute
		}
	case errors.Is(err, api.ErrQuota), errors.Is(err, api.ErrForbidden), errors.Is(err, localfs.ErrNotEmpty):
		d = 10 * time.Minute
	default:
		d = min(time.Second<<min(b.attempts, 9), 5*time.Minute)
	}
	if d > 0 {
		// Jitter, so ops that failed together don't retry together.
		d = d/2 + rand.N(d/2+1)
	}
	b.until = time.Now().Add(d)
	return b
}

func (e *Engine) succeeded(o Op) {
	e.mu.Lock()
	delete(e.backoff, o.key())
	e.mu.Unlock()
}

// resumeJournal runs what the journal holds before anything new is
// planned: on the first pass, the ops a crash interrupted (their stale
// .part files are cleaned first); later, transfers due for a retry.
func (e *Engine) resumeJournal(ctx, outer context.Context, res *PassResult) error {
	first := !e.scanned
	if first {
		if err := e.cleanParts(ctx); err != nil {
			return err
		}
	}
	var due []state.Op
	err := e.db.View(ctx, func(tx *state.Tx) error {
		var err error
		if first {
			due, err = tx.Ops()
		} else {
			due, err = tx.DueOps(time.Now(), 1000)
		}
		return err
	})
	if err != nil {
		return err
	}
	for _, so := range due {
		o, err := decodeOp(so)
		if err != nil {
			e.log.Error("sync: dropping an unreadable op", "id", so.ID, "err", err)
			if err := e.db.Update(ctx, func(tx *state.Tx) error { return tx.DeleteOp(so.ID) }); err != nil {
				return err
			}
			continue
		}
		if first {
			e.log.Info("sync: finishing an interrupted op", "op", o.Kind, "path", o.Path)
		}
		if e.background(o) {
			if !e.startBackground(outer, so.ID, o, false) {
				// No slot now: due again on a later pass (an op found
				// running after a crash isn't due until made pending).
				if err := e.db.Update(ctx, func(tx *state.Tx) error { return tx.RetryOp(so.ID) }); err != nil {
					return err
				}
				res.Deferred++
			}
			continue
		}
		if err := e.run(ctx, so.ID, o, res, false); err != nil {
			return err
		}
	}
	return nil
}

func decodeOp(so state.Op) (Op, error) {
	var o Op
	if err := json.Unmarshal(so.Params, &o); err != nil {
		return Op{}, err
	}
	o.Kind, o.NodeID, o.Path = OpKind(so.Kind), so.NodeID, so.Path
	return o, nil
}

// pathLocks: an op holds its path, and no other op works at, above or
// below it meanwhile.
type pathLocks struct {
	mu   sync.Mutex
	cond *sync.Cond
	held map[string]bool
}

func newPathLocks() *pathLocks {
	l := &pathLocks{held: map[string]bool{}}
	l.cond = sync.NewCond(&l.mu)
	return l
}

func (l *pathLocks) lock(p string) func() {
	l.mu.Lock()
	for l.conflicts(p) {
		l.cond.Wait()
	}
	l.held[p] = true
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.held, p)
		l.cond.Broadcast()
		l.mu.Unlock()
	}
}

func (l *pathLocks) conflicts(p string) bool {
	for h := range l.held {
		if under(p, h) || under(h, p) {
			return true
		}
	}
	return false
}

// notExist is a missing file or folder, from os or localfs — also one
// whose folder on the way is a file now (ENOTDIR).
func notExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}
