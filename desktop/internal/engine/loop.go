package engine

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/do-service/cloud/desktop/internal/watch"
)

// maxPasses bounds SyncOnce: each pass does at least one op, and a tree
// converges in about its depth's worth of passes; more means ping-pong.
const maxPasses = 200

// ErrNoProgress: SyncOnce ran maxPasses passes and still had work.
var ErrNoProgress = errors.New("engine: sync doesn't converge")

// SyncOnce runs passes until one has nothing to do: what's left then
// waits for something outside (a backoff, the user, the other side). It
// waits for files still being written and for large transfers running in
// the background. It starts with a full scan: run without a watcher, it
// can't know what changed since the last run. Paused, it returns
// ErrPaused.
func (e *Engine) SyncOnce(ctx context.Context) (PassResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		e.Wait()
	}()
	e.RescanAll()
	e.pullMu.Lock()
	e.fresh = false
	e.pullMu.Unlock()
	var total PassResult
	for range maxPasses {
		res, err := e.Pass(ctx)
		total.Ops += res.Ops
		total.Failed += res.Failed
		total.Deferred, total.Unstable, total.HeldDeletes = res.Deferred, res.Unstable, res.HeldDeletes
		total.Pending, total.Problems = res.Pending, res.Problems
		if err != nil {
			return total, err
		}
		if res.Ops > 0 {
			continue
		}
		switch {
		case res.Unstable > 0:
			if err := sleep(ctx, max(e.cfg.Options.StableAfter/2, 100*time.Millisecond)); err != nil {
				return total, err
			}
		case e.bgUncollected():
			// Ended just after the pass: one more for its follow-ups.
		case e.bgRunning() > 0:
			// A pass when one is done: its result may have follow-ups.
			if err := sleepWake(ctx, time.Minute, e.wake); err != nil {
				return total, err
			}
		default:
			return total, nil
		}
	}
	return total, ErrNoProgress
}

// Run syncs until ctx ends: a pass whenever the watcher reports changes
// or the server's, passes back to back while they do something. Idle, it
// long-polls the server's changes (Config.Wait), cut short by a local
// change. Without a watcher (Windows for now, or no cgo on macOS) the
// folder is rescanned every FallbackRescan. A pass failing for the
// network is retried with backoff (offline, Status); paused, Run waits
// for Resume — also one made by another process (the flag in state.db).
// ErrNoMarker and ErrNeedsSignIn end Run.
func (e *Engine) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		e.Wait()
	}()
	w, err := watch.New(e.cfg.Root, watch.Options{Since: watch.SinceNow})
	var local <-chan struct{}
	switch {
	case errors.Is(err, watch.ErrUnsupported):
		e.log.Info("sync: no file system watcher here; rescanning periodically", "every", e.cfg.FallbackRescan)
	case err != nil:
		return err
	default:
		defer w.Close()
		e.mu.Lock()
		e.watching = true
		e.mu.Unlock()
		local = e.drain(ctx, watch.Debounce(w.Root(), w.Events(), time.Second, 2*time.Second))
	}
	go e.followPaused(ctx)
	fails := 0
	for {
		if e.isPaused() {
			if sleepWake(ctx, time.Hour, e.wake) != nil {
				return nil
			}
			continue
		}
		res, err := e.Pass(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, ErrPaused), err != nil && e.isPaused():
			continue
		case errors.Is(err, ErrNoMarker), errors.Is(err, ErrNeedsSignIn):
			return err
		case err != nil:
			fails++
			if e.offline(ctx, err, &fails, local) != nil {
				return nil
			}
			continue
		}
		fails = 0
		if res.Ops > 0 {
			continue
		}
		if res.Unstable > 0 {
			if sleepWake(ctx, max(e.cfg.Options.StableAfter/2, 100*time.Millisecond), local, e.wake) != nil {
				return nil
			}
			continue
		}
		if e.idle(ctx, local) != nil {
			return nil
		}
	}
}

// retryDelay after fails failures in a row: 1 s doubling up to 30 s,
// with jitter (half to all of it), so clients of a server that comes back
// don't return all at once. Capped low: a network that comes back is
// noticed within half a minute.
func retryDelay(fails int) time.Duration {
	d := min(time.Second<<min(fails-1, 5), 30*time.Second)
	return d/2 + rand.N(d/2+1)
}

// pinger is a Remote that can tell whether the server answers with one
// cheap request, without the client's retries (api.Client.Ping).
type pinger interface {
	Ping(ctx context.Context) error
}

// offline waits out a failed pass: with backoff, woken early by a local
// change; while the server doesn't answer a ping, it keeps waiting
// without running a whole pass (whose requests retry for a while each).
func (e *Engine) offline(ctx context.Context, err error, fails *int, local <-chan struct{}) error {
	for {
		d := retryDelay(*fails)
		if transient(err) {
			e.setOffline(ctx, err, time.Now().Add(d))
		}
		e.log.Warn("sync: pass failed; retrying", "err", err, "in", d.Round(time.Second))
		if err := sleepWake(ctx, d, local, e.wake); err != nil {
			return err
		}
		p, ok := e.r.(pinger)
		if !ok || e.isPaused() {
			return nil
		}
		if err = p.Ping(ctx); err == nil || ctx.Err() != nil {
			return ctx.Err()
		}
		*fails++
	}
}

// followPaused picks up a pause or resume made by another process
// (cloudsync pause): it is only in the database.
func (e *Engine) followPaused(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.loadPaused(ctx); err != nil && ctx.Err() == nil {
				e.log.Warn("sync: reading the pause flag failed", "err", err)
			}
		}
	}
}

// idle waits for the next pass: the server's changes (a long poll), a
// local change, a background transfer done, a resume — or PollInterval
// at least since the last poll, which paces a server that answers a long
// poll at once. A pending poll never holds up a local change: it is cut
// off.
func (e *Engine) idle(ctx context.Context, local <-chan struct{}) error {
	start := time.Now()
	pctx, cancel := context.WithCancel(ctx)
	got := make(chan bool, 1)
	go func() { got <- e.pollRemote(pctx) }()
	var changed, answered bool
	select {
	case <-ctx.Done():
	case <-local:
	case <-e.wake:
	case changed = <-got:
		answered = true
	}
	cancel()
	if !answered {
		// The poll touches the mirror: it ends before the pass starts.
		<-got
		return ctx.Err()
	}
	if rest := e.cfg.PollInterval - time.Since(start); !changed && rest > 0 {
		return sleepWake(ctx, rest, local, e.wake)
	}
	return ctx.Err()
}

// drain hands watcher bursts to MarkDirty as they come, also during a
// pass, so the watcher never waits on the engine; the channel it returns
// wakes the loop.
func (e *Engine) drain(ctx context.Context, dirty <-chan watch.Dirty) <-chan struct{} {
	// Never closed: a stopped watcher leaves the loop to its poll timer.
	wake := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case d, ok := <-dirty:
				if !ok {
					return
				}
				e.MarkDirty(d)
				select {
				case wake <- struct{}{}:
				default:
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return wake
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// sleepWake sleeps d, or until a wake channel (nil ones never) has
// something.
func sleepWake(ctx context.Context, d time.Duration, wake <-chan struct{}, more ...<-chan struct{}) error {
	t := time.NewTimer(d)
	defer t.Stop()
	var other <-chan struct{}
	if len(more) > 0 {
		other = more[0]
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
	case <-wake:
	case <-other:
	}
	return nil
}
