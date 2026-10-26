package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/do-service/cloud/desktop/internal/state"
)

// ErrPaused: the user paused sync (Pause, cloudsync pause); Pass and
// SyncOnce do nothing until Resume.
var ErrPaused = errors.New("engine: sync is paused")

// Status is what the engine is doing, for the app's tray and the CLI.
type Status struct {
	Paused bool
	// Offline: the last pass couldn't reach the server; it is asked
	// again at NextTry (sooner on a local change).
	Offline      bool
	OfflineSince time.Time
	NextTry      time.Time
	// Transfers: large transfers running in the background.
	Transfers int
}

func (e *Engine) Status() Status {
	e.ctl.Lock()
	defer e.ctl.Unlock()
	return Status{Paused: e.paused, Offline: !e.offlineSince.IsZero(), OfflineSince: e.offlineSince, NextTry: e.nextTry,
		Transfers: len(e.bgPaths)}
}

// Pause stops sync: transfers under way are cut off (a resumable one
// carries on from where it got after Resume), no pass starts. It is
// recorded in the state database, so a running engine of another process
// (cloudsync sync -watch) and the next start see it too.
func (e *Engine) Pause(ctx context.Context) error {
	if err := e.db.Update(ctx, func(tx *state.Tx) error { return tx.SetPaused(true) }); err != nil {
		return err
	}
	e.setPaused(true)
	return nil
}

// Resume undoes Pause.
func (e *Engine) Resume(ctx context.Context) error {
	if err := e.db.Update(ctx, func(tx *state.Tx) error { return tx.SetPaused(false) }); err != nil {
		return err
	}
	e.setPaused(false)
	return nil
}

func (e *Engine) setPaused(p bool) {
	e.ctl.Lock()
	defer e.ctl.Unlock()
	if e.paused == p {
		return
	}
	e.paused = p
	if p {
		for _, cancel := range e.working {
			cancel()
		}
	}
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// loadPaused takes the flag from the database: another process may have
// set it.
func (e *Engine) loadPaused(ctx context.Context) error {
	var p bool
	err := e.db.View(ctx, func(tx *state.Tx) error {
		var err error
		p, err = tx.Paused()
		return err
	})
	if err == nil {
		e.setPaused(p)
	}
	return err
}

// work is ctx that Pause cancels; done releases it.
func (e *Engine) work(ctx context.Context) (context.Context, func(), error) {
	e.ctl.Lock()
	defer e.ctl.Unlock()
	if e.paused {
		return nil, nil, ErrPaused
	}
	ctx, cancel := context.WithCancel(ctx)
	e.workID++
	id := e.workID
	e.working[id] = cancel
	return ctx, func() {
		e.ctl.Lock()
		delete(e.working, id)
		e.ctl.Unlock()
		cancel()
	}, nil
}

func (e *Engine) isPaused() bool {
	e.ctl.Lock()
	defer e.ctl.Unlock()
	return e.paused
}

// Large transfers run in the background, past the pass that started
// them: a 2 GB upload mustn't hold up the small files and the passes
// after it. Their op is in the journal from the start, so the planner
// leaves the node alone (busy), and ops of later passes on a path at,
// above or below one of them wait for it (bgBlocks): a folder isn't
// moved or removed under a transfer.

// bgBlocks reports whether a running background transfer holds a path
// of o.
func (e *Engine) bgBlocks(o Op) bool {
	e.ctl.Lock()
	defer e.ctl.Unlock()
	if len(e.bgPaths) == 0 {
		return false
	}
	paths := []string{o.Path, o.From, o.BasePath}
	for _, it := range o.Items {
		paths = append(paths, it.Path)
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		for h := range e.bgPaths {
			if under(p, h) || under(h, p) {
				return true
			}
		}
	}
	return false
}

// background reports whether o is a transfer to run in the background.
func (e *Engine) background(o Op) bool {
	return (o.Kind == OpUpload || o.Kind == OpDownload) && o.Size >= e.cfg.LargeSize
}

// startBackground runs journaled op id in the background if a large
// slot is free, and reports whether it did. ctx is the caller's of Pass:
// the transfer lives until it ends, Pause, or the transfer is done.
func (e *Engine) startBackground(ctx context.Context, id int64, o Op, started bool) bool {
	e.ctl.Lock()
	if len(e.bgPaths) >= e.cfg.LargeTransfers || e.bgPaths[o.Path] {
		e.ctl.Unlock()
		return false
	}
	e.bgPaths[o.Path] = true
	e.ctl.Unlock()
	wctx, done, err := e.work(ctx)
	if err != nil {
		e.ctl.Lock()
		delete(e.bgPaths, o.Path)
		e.ctl.Unlock()
		return false
	}
	e.bg.Add(1)
	go func() {
		defer e.bg.Done()
		defer done()
		var r PassResult
		err := e.run(wctx, id, o, &r, started)
		e.ctl.Lock()
		delete(e.bgPaths, o.Path)
		e.bgRes.Ops += r.Ops
		e.bgRes.Failed += r.Failed
		if err != nil && wctx.Err() == nil && e.bgErr == nil {
			e.bgErr = err
		}
		e.ctl.Unlock()
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}()
	return true
}

// collectBackground adds what background transfers did since the last
// pass to res, and returns a fatal error one of them hit.
func (e *Engine) collectBackground(res *PassResult) error {
	e.ctl.Lock()
	defer e.ctl.Unlock()
	res.Ops += e.bgRes.Ops
	res.Failed += e.bgRes.Failed
	e.bgRes = PassResult{}
	err := e.bgErr
	e.bgErr = nil
	return err
}

func (e *Engine) bgRunning() int {
	e.ctl.Lock()
	defer e.ctl.Unlock()
	return len(e.bgPaths)
}

// bgUncollected: a background transfer ended after the last pass looked.
func (e *Engine) bgUncollected() bool {
	e.ctl.Lock()
	defer e.ctl.Unlock()
	return e.bgRes.Ops > 0 || e.bgErr != nil
}

// Wait waits for the background transfers to end: call it (after
// cancelling Pass's context, or when they are done) before closing the
// database. SyncOnce and Run do it themselves.
func (e *Engine) Wait() { e.bg.Wait() }

// offline bookkeeping: a pass that failed for the network marks the
// connection offline (Status, and a problem the CLI's status reads);
// the first pass that gets through clears it.
func (e *Engine) setOffline(ctx context.Context, err error, next time.Time) {
	e.ctl.Lock()
	first := e.offlineSince.IsZero()
	if first {
		e.offlineSince = time.Now()
	}
	e.nextTry = next
	e.ctl.Unlock()
	if first {
		_ = e.db.Update(context.WithoutCancel(ctx), func(tx *state.Tx) error {
			_, perr := tx.PutProblem(state.Problem{Kind: ProblemOffline, Detail: err.Error()})
			return perr
		})
	}
}

func (e *Engine) setOnline(ctx context.Context) error {
	e.ctl.Lock()
	// Once per start too: a run that ended offline left the problem.
	clear := !e.offlineSince.IsZero() || !e.onlineSeen
	e.offlineSince, e.nextTry, e.onlineSeen = time.Time{}, time.Time{}, true
	e.ctl.Unlock()
	if !clear {
		return nil
	}
	return e.db.Update(ctx, func(tx *state.Tx) error { return tx.ResolveProblems(ProblemOffline, "") })
}

// control is the part of Engine above, under its own lock: Status is
// read by the app's UI while passes run.
type control struct {
	ctl     sync.Mutex
	paused  bool
	working map[int64]context.CancelFunc
	workID  int64
	// wake nudges Run: a resume, a background transfer done.
	wake chan struct{}

	bg      sync.WaitGroup
	bgPaths map[string]bool
	bgRes   PassResult
	bgErr   error

	offlineSince, nextTry time.Time
	onlineSeen            bool
}
