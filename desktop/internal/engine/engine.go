// Package engine is the sync engine of the desktop client
// (docs/desktop-client.md, «Движок»): a mirror of the server's tree
// (remote, from /sync/tree and /sync/changes), the last scan of the sync
// folder (local) and what both last agreed on (base), reconciled three
// ways into a plan of ops (MakePlan, a pure function), which the executor
// runs through the state's op journal so a crash at any point is redone
// safely.
//
// One Engine syncs one connection. It doesn't import Wails: the CLI
// (cmd/cloudsync) and the app both drive it, and it builds and tests with
// CGO_ENABLED=0.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

var (
	// ErrNeedsSignIn: the server refused the device token (revoked, or
	// the account blocked). The engine stops; sign in again.
	ErrNeedsSignIn = errors.New("engine: signed out on the server; sign in again")
	// ErrNoMarker: the sync folder lost its .cloudsync (a disk unplugged,
	// the folder moved). Nothing is synced, deletes least of all.
	ErrNoMarker = errors.New("engine: the sync folder's marker is missing")
	// errStop is a Config.Hook's way to end a pass as if the process
	// died: nothing more is recorded.
	errStop = errors.New("engine: stopped by the test hook")
)

type Config struct {
	DB     *state.DB
	Remote Remote
	// Root is the sync folder, absolute; ConnID the connection whose
	// marker it must have.
	Root   string
	ConnID string
	// UserID is the account's.
	UserID  string
	Options Options

	// PollInterval is the least time between two polls of /sync/changes
	// when idle (default 5 s): what paces them with a server that answers
	// a long poll at once (one that ignores wait).
	PollInterval time.Duration
	// Wait is the long poll's wait passed to Changes when idle (default
	// 55 s, capped by the server's /sync/info maxWait when the Remote
	// tells it); a negative Wait polls without one.
	Wait time.Duration
	// RescanInterval: a full rescan this often anyway (default 1 h);
	// FallbackRescan without a watcher (default 1 min).
	RescanInterval, FallbackRescan time.Duration

	// The mass-delete guard: more than MassDeleteFiles files, or at least
	// MassDeleteMin and more than MassDeleteShare of the synced files,
	// disappearing in a pass waits for ConfirmDeletes.
	MassDeleteFiles int
	MassDeleteMin   int
	MassDeleteShare float64
	ConfirmDeletes  bool

	// Transfers in parallel: LargeTransfers of files from LargeSize up,
	// in the background across passes, SmallTransfers of the rest within
	// a pass (defaults 3, 8, 16 MiB).
	LargeTransfers, SmallTransfers int
	LargeSize                      int64
	// LockRetryFor: a file locked on the server is retried every
	// LockBackoff (default 30 s) this long before it becomes a problem
	// (default 30 min).
	LockRetryFor, LockBackoff time.Duration

	Logger *slog.Logger
	// Hook, for tests, is called with "act" before an op's action and
	// "acted" after it, before its result is recorded; an error ends the
	// pass right there, as a crash would.
	Hook func(stage string, op Op) error
}

func (c *Config) defaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.Wait == 0 {
		c.Wait = 55 * time.Second
	}
	if c.RescanInterval <= 0 {
		c.RescanInterval = time.Hour
	}
	if c.FallbackRescan <= 0 {
		c.FallbackRescan = time.Minute
	}
	if c.MassDeleteFiles <= 0 {
		c.MassDeleteFiles = 500
	}
	if c.MassDeleteMin <= 0 {
		c.MassDeleteMin = 10
	}
	if c.MassDeleteShare <= 0 {
		c.MassDeleteShare = 0.3
	}
	if c.LargeTransfers <= 0 {
		c.LargeTransfers = 3
	}
	if c.SmallTransfers <= 0 {
		c.SmallTransfers = 8
	}
	if c.LargeSize <= 0 {
		c.LargeSize = 16 << 20
	}
	if c.LockRetryFor <= 0 {
		c.LockRetryFor = 30 * time.Minute
	}
	if c.LockBackoff <= 0 {
		c.LockBackoff = 30 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Engine syncs one connection. Passes are not to be run concurrently;
// the methods of selection.go and browse.go may run alongside them once
// Prepare has succeeded (see Prepare); Status, Pause, Resume and Wake
// may be called any time.
type Engine struct {
	control
	cfg Config
	db  *state.DB
	r   Remote
	log *slog.Logger

	rootID string

	// pullMu serializes pulls of the mirror (a folder trash pulls in the
	// middle of a pass) with the recording of op results, which write the
	// mirror too: a page of changes read before a background transfer's
	// result and applied after it would put the node's old state back.
	// mirrorGen counts those results, for the idle long poll, which
	// doesn't hold the lock while it waits; fresh: it just brought the
	// mirror up to date.
	pullMu    sync.Mutex
	mirrorGen uint64
	fresh     bool
	// maxWait is the server's cap of the long poll; -1 when it has none.
	maxWait time.Duration

	mu       sync.Mutex
	backoff  map[string]*backoff
	dirty    map[string]bool
	fullScan bool
	scanned  bool
	lastFull time.Time
	watching bool

	// sync batches the flushes of local changes (localfs.Syncer): an op
	// that changed the disk waits for it before its result is recorded.
	sync *localfs.Syncer
}

func New(cfg Config) (*Engine, error) {
	if cfg.DB == nil || cfg.Remote == nil || cfg.Root == "" || cfg.ConnID == "" || cfg.UserID == "" {
		return nil, errors.New("engine: DB, Remote, Root, ConnID and UserID are required")
	}
	cfg.defaults()
	e := &Engine{cfg: cfg, db: cfg.DB, r: cfg.Remote, log: cfg.Logger, backoff: map[string]*backoff{}, dirty: map[string]bool{},
		sync: localfs.NewSyncer()}
	e.working, e.bgPaths, e.wake = map[int64]context.CancelFunc{}, map[string]bool{}, make(chan struct{}, 1)
	if err := e.db.View(context.Background(), func(tx *state.Tx) error {
		roots, err := tx.Roots()
		for _, r := range roots {
			if r.Kind == state.RootOwn {
				e.rootID = r.NodeID
			}
		}
		return err
	}); err != nil {
		return nil, err
	}
	if err := e.loadPaused(context.Background()); err != nil {
		return nil, err
	}
	return e, nil
}

// PassResult says what a pass did.
type PassResult struct {
	// Ops run (whatever their outcome) and Failed of them.
	Ops, Failed int
	// Deferred and Unstable as in Plan.
	Deferred, Unstable int
	// HeldDeletes: files the mass-delete guard kept from disappearing.
	HeldDeletes int
	// Pending: ops left in the journal, backing off.
	Pending  int
	Problems int
}

// Pass is one round: check the marker, pull the server's changes, finish
// what the journal holds, rescan what changed locally, plan, run the
// plan. It returns ErrNoMarker and ErrNeedsSignIn as such, ErrPaused
// while paused; other errors (the server unreachable) are worth retrying
// later. Large transfers it starts go on in the background (control.go)
// with ctx: Wait for them before closing the database.
func (e *Engine) Pass(outer context.Context) (PassResult, error) {
	var res PassResult
	ctx, done, err := e.work(outer)
	if err != nil {
		return res, err
	}
	defer done()
	if err := e.collectBackground(&res); err != nil {
		return res, e.fatal(ctx, err)
	}
	if err := e.checkMarker(ctx); err != nil {
		return res, err
	}
	if err := e.pullRemote(ctx); err != nil {
		return res, e.fatal(ctx, err)
	}
	if err := e.setOnline(ctx); err != nil {
		return res, err
	}
	if err := e.resumeJournal(ctx, outer, &res); err != nil {
		return res, e.fatal(ctx, err)
	}
	if err := e.refreshLocal(ctx); err != nil {
		return res, err
	}
	// Again after the scan: a disk unplugged during it looks like an
	// empty folder.
	if err := e.checkMarker(ctx); err != nil {
		return res, err
	}
	snap, synced, err := e.snapshot(ctx)
	if err != nil {
		return res, err
	}
	plan := MakePlan(snap)
	res.Deferred, res.Unstable = plan.Deferred, plan.Unstable
	res.HeldDeletes = e.guard(ctx, &plan, synced)
	if err := e.syncProblems(ctx, plan.Problems); err != nil {
		return res, err
	}
	if err := e.execute(ctx, outer, plan.Ops, &res); err != nil {
		return res, e.fatal(ctx, err)
	}
	err = e.db.View(ctx, func(tx *state.Tx) error {
		ops, err := tx.Ops()
		res.Pending = len(ops)
		if err != nil {
			return err
		}
		ps, err := tx.Problems()
		res.Problems = len(ps)
		return err
	})
	return res, err
}

// fatal records ErrNeedsSignIn as a problem the CLI and the app show.
func (e *Engine) fatal(ctx context.Context, err error) error {
	if errors.Is(err, api.ErrUnauthorized) || errors.Is(err, ErrNeedsSignIn) {
		_ = e.db.Update(context.WithoutCancel(ctx), func(tx *state.Tx) error {
			_, err := tx.PutProblem(state.Problem{Kind: ProblemNeedsSignIn, Detail: "the device was signed out on the server"})
			return err
		})
		return ErrNeedsSignIn
	}
	return err
}

func (e *Engine) checkMarker(ctx context.Context) error {
	if err := localfs.CheckMarker(e.cfg.Root, e.cfg.ConnID); err != nil {
		_ = e.db.Update(ctx, func(tx *state.Tx) error {
			_, perr := tx.PutProblem(state.Problem{Kind: ProblemNoMarker, RelPath: "", Detail: err.Error()})
			return perr
		})
		return fmt.Errorf("%w: %v", ErrNoMarker, err)
	}
	return e.db.Update(ctx, func(tx *state.Tx) error {
		if err := tx.ResolveProblems(ProblemNoMarker, ""); err != nil {
			return err
		}
		return tx.ResolveProblems(ProblemNeedsSignIn, "")
	})
}

// syncProblems replaces the planner's problems with this pass's.
func (e *Engine) syncProblems(ctx context.Context, found []state.Problem) error {
	type key struct{ kind, path, node string }
	want := map[key]state.Problem{}
	for _, p := range found {
		want[key{p.Kind, p.RelPath, p.NodeID}] = p
	}
	planned := map[string]bool{}
	for _, k := range plannerProblems {
		planned[k] = true
	}
	return e.db.Update(ctx, func(tx *state.Tx) error {
		have, err := tx.Problems()
		if err != nil {
			return err
		}
		for _, p := range have {
			k := key{p.Kind, p.RelPath, p.NodeID}
			if _, ok := want[k]; ok {
				delete(want, k)
				continue
			}
			if planned[p.Kind] {
				if err := tx.DeleteProblem(p.ID); err != nil {
					return err
				}
			}
		}
		for _, p := range want {
			if _, err := tx.PutProblem(p); err != nil {
				return err
			}
		}
		return nil
	})
}

// guard holds back the deletes of a plan that would make too many files
// disappear at once (docs/desktop-client.md, «Защита»), until the user
// confirms; the rest of the plan runs. synced is how many files base has.
func (e *Engine) guard(ctx context.Context, plan *Plan, synced int) int {
	n := plan.Deletes()
	if n == 0 {
		return 0
	}
	if !(n > e.cfg.MassDeleteFiles || n >= e.cfg.MassDeleteMin && float64(n) > e.cfg.MassDeleteShare*float64(synced)) {
		return 0
	}
	if e.cfg.ConfirmDeletes {
		e.log.Warn("sync: mass delete confirmed", "files", n)
		_ = e.db.Update(ctx, func(tx *state.Tx) error { return tx.ResolveProblems(ProblemMassDelete, "") })
		return 0
	}
	kept := plan.Ops[:0]
	for _, o := range plan.Ops {
		if o.Deletes == 0 {
			kept = append(kept, o)
		}
	}
	plan.Ops = kept
	e.log.Warn("sync: deletes paused until confirmed", "files", n, "synced", synced)
	_ = e.db.Update(ctx, func(tx *state.Tx) error {
		_, err := tx.PutProblem(state.Problem{Kind: ProblemMassDelete, Detail: fmt.Sprintf("%d files would be deleted", n)})
		return err
	})
	return n
}
