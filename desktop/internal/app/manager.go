package app

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/connection"
	"github.com/do-service/cloud/desktop/internal/engine"
	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/state"
)

// Options configure a Manager.
type Options struct {
	// Base is the state directory (state.BaseDir()); Store keeps the
	// tokens.
	Base  string
	Store *signin.Store
	// OpenBrowser opens the sign-in page (signin.OpenBrowser when nil);
	// OpenFolder shows a folder in Finder or its kin.
	OpenBrowser func(url string) error
	OpenFolder  func(path string) error
	// PickFolder asks the user for a folder in the OS's dialog, starting
	// in start; "" when cancelled. Nil: no dialog (errors.ErrUnsupported).
	PickFolder func(start string) (string, error)
	// Tune adjusts each engine's config (tests).
	Tune func(*engine.Config)
	// VersionCheck is how often a syncing connection asks the server
	// whether it still takes this client (minClientVersion), and how
	// often one it turned away asks again; DefaultVersionCheck when 0.
	VersionCheck time.Duration
	Logger       *slog.Logger
}

// DefaultVersionCheck is Options.VersionCheck's default.
const DefaultVersionCheck = time.Hour

// Manager runs the app's connections: one engine per connection in this
// process, as cloudsync sync -watch would, each under the connection's
// lock, so the CLI or a second app can't sync it at the same time.
type Manager struct {
	opts Options
	reg  *state.Registry
	ctx  context.Context
	stop context.CancelFunc
	log  *slog.Logger

	mu      sync.Mutex
	runners map[string]*runner
	order   []string
	active  string
	signin  *signinState
	// moving: connections whose folder MoveFolder is moving.
	moving map[string]bool
	// view: what the tray asked the window to show (RequestView).
	view string
	wg   sync.WaitGroup
}

// NewManager starts the engines of the connections in the registry.
func NewManager(opts Options) (*Manager, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.OpenBrowser == nil {
		opts.OpenBrowser = signin.OpenBrowser
	}
	if opts.VersionCheck <= 0 {
		opts.VersionCheck = DefaultVersionCheck
	}
	ctx, stop := context.WithCancel(context.Background())
	m := &Manager{opts: opts, reg: state.OpenRegistry(opts.Base), ctx: ctx, stop: stop, log: opts.Logger, runners: map[string]*runner{}, moving: map[string]bool{}}
	conns, err := m.reg.List()
	if err != nil {
		stop()
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range conns {
		m.startLocked(c)
	}
	if len(conns) > 0 {
		m.active = conns[0].ID
	}
	return m, nil
}

// Close stops every engine and waits for them; transfers under way are
// cut off and resume next time.
func (m *Manager) Close() {
	m.stop()
	m.wg.Wait()
}

func (m *Manager) startLocked(c state.Connection) {
	r := &runner{m: m, conn: c, phase: phaseStarting, done: make(chan struct{})}
	var ctx context.Context
	ctx, r.cancel = context.WithCancel(m.ctx)
	if _, ok := m.runners[c.ID]; !ok {
		m.order = append(m.order, c.ID)
	}
	m.runners[c.ID] = r
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(r.done)
		r.loop(ctx)
	}()
}

func (m *Manager) runner(id string) (*runner, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runners[id]
	return r, ok
}

func (m *Manager) runnersInOrder() []*runner {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*runner, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.runners[id])
	}
	return out
}

// SetActive picks the connection the window shows, which its /api goes
// to (Upstream).
func (m *Manager) SetActive(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runners[id]; !ok {
		return state.ErrNotFound
	}
	m.active = id
	return nil
}

// Upstream is the proxy's target: the active connection's server and
// token.
func (m *Manager) Upstream() (Upstream, error) {
	m.mu.Lock()
	r, ok := m.runners[m.active]
	m.mu.Unlock()
	if !ok {
		return Upstream{}, ErrNoUpstream
	}
	u, err := url.Parse(r.connection().Server)
	if err != nil {
		return Upstream{}, err
	}
	return Upstream{Server: u, Token: r.tokenSource()}, nil
}

// Remove disconnects a connection (connection.Remove): its engine stops
// first. The files stay in the folder.
func (m *Manager) Remove(ctx context.Context, id string, force bool) error {
	r, ok := m.runner(id)
	if !ok {
		return state.ErrNotFound
	}
	r.cancel()
	<-r.done
	if err := connection.Remove(ctx, m.opts.Base, m.opts.Store, r.connection(), force); err != nil {
		// Still connected: sync goes on.
		m.mu.Lock()
		if m.runners[id] == r {
			m.startLocked(r.connection())
		}
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.runners, id)
	for i, o := range m.order {
		if o == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	if m.active == id {
		m.active = ""
		if len(m.order) > 0 {
			m.active = m.order[0]
		}
	}
	return nil
}

// OpenFolder shows a connection's sync folder.
func (m *Manager) OpenFolder(id string) error {
	r, ok := m.runner(id)
	if !ok {
		return state.ErrNotFound
	}
	if m.opts.OpenFolder == nil {
		return errors.ErrUnsupported
	}
	return m.opts.OpenFolder(r.connection().LocalRoot)
}

// PickFolder shows the OS's folder dialog (Options.PickFolder), in the
// nearest folder of start that exists: the default sync folder may not
// yet.
func (m *Manager) PickFolder(start string) (string, error) {
	if m.opts.PickFolder == nil {
		return "", errors.ErrUnsupported
	}
	for start != "" {
		if fi, err := os.Stat(start); err == nil && fi.IsDir() {
			break
		}
		up := filepath.Dir(start)
		if up == start {
			start = ""
			break
		}
		start = up
	}
	return m.opts.PickFolder(start)
}

// MoveFolder moves a connection's sync folder to dir («Сменить папку»,
// connection.MoveFolder). A folder that can't take it is refused before
// sync is touched; then the engine stops (transfers under way are cut
// off and resume after), the folder moves, and the engine starts again
// from the registry — in the new place, or in the old one if the move
// was undone.
func (m *Manager) MoveFolder(ctx context.Context, id, dir string) (state.Connection, error) {
	r, ok := m.runner(id)
	if !ok {
		return state.Connection{}, state.ErrNotFound
	}
	conn := r.connection()
	if _, err := connection.CheckMoveTarget(m.opts.Base, conn, dir); err != nil {
		return conn, err
	}
	m.mu.Lock()
	if m.runners[id] != r || m.moving[id] {
		m.mu.Unlock()
		return conn, state.ErrLocked
	}
	m.moving[id] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.moving, id)
		m.mu.Unlock()
	}()
	r.setPhase(phaseMoving)
	r.cancel()
	<-r.done
	r.setPhase(phaseMoving)
	// Not cut short by the window going away: a move is finished.
	moved, err := connection.MoveFolder(context.WithoutCancel(ctx), m.opts.Base, conn, dir)
	if c, gerr := m.reg.Get(id); gerr == nil {
		moved = c
	}
	m.mu.Lock()
	if m.runners[id] == r {
		m.startLocked(moved)
	}
	m.mu.Unlock()
	return moved, err
}

// ErrNotRunning: the connection's engine isn't open (another process has
// it, or it is starting), so it can't be asked.
var ErrNotRunning = errors.New("app: the connection isn't syncing here")

// SetPaused pauses or resumes one connection, or all with id "".
func (m *Manager) SetPaused(ctx context.Context, id string, paused bool) error {
	var rs []*runner
	if id == "" {
		rs = m.runnersInOrder()
	} else {
		r, ok := m.runner(id)
		if !ok {
			return state.ErrNotFound
		}
		rs = []*runner{r}
	}
	var first error
	for _, r := range rs {
		err := r.withEngine(func(e *engine.Engine) error {
			if paused {
				return e.Pause(ctx)
			}
			return e.Resume(ctx)
		})
		if err != nil && first == nil && id != "" {
			first = err
		}
	}
	return first
}

// phase is where a runner's engine is.
type phase string

const (
	phaseStarting   phase = "starting"
	phaseRunning    phase = "running"
	phaseBusy       phase = "busy"         // another process has the connection
	phaseNeedSignIn phase = "needs_signin" // the device was signed out on the server
	phaseNoFolder   phase = "no_folder"    // the sync folder's marker is gone
	phaseMoving     phase = "moving"       // the sync folder is being moved
	phaseTooOld     phase = "too_old"      // the server asks for a newer client
	phaseFailed     phase = "error"
)

// tooOldError: the server's minClientVersion is above this client's.
type tooOldError struct{ min string }

func (e *tooOldError) Error() string {
	return "app: the server needs client version " + e.min + " or newer"
}

// runner keeps one connection's engine running, opening it again after
// what can pass: another process letting go, the folder coming back, the
// server answering.
type runner struct {
	m *Manager
	// conn is the loop's; others read it with connection(), since Open
	// may change its folder (a move a crash left unfinished).
	conn   state.Connection
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.RWMutex
	sess  *connection.Session
	ready bool // Prepare succeeded: the mirror is there to browse
	phase phase
	// minVersion is the server's minClientVersion in phaseTooOld.
	minVersion string
	// tok is the store's TokenSource for the proxy, made once: it reads
	// the keyring on first use only.
	tokOnce sync.Once
	tok     api.TokenSource
}

func (r *runner) tokenSource() api.TokenSource {
	r.tokOnce.Do(func() {
		c := r.connection()
		r.tok = r.m.opts.Store.TokenSource(c.Server, c.UserID)
	})
	return r.tok
}

func (r *runner) connection() state.Connection {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conn
}

func isUnauthorized(err error) bool { return errors.Is(err, api.ErrUnauthorized) }

func (r *runner) setPhase(p phase) {
	r.mu.Lock()
	r.phase = p
	r.mu.Unlock()
}

func (r *runner) loop(ctx context.Context) {
	log := r.m.log.With("conn", r.conn.ID)
	fails := 0
	for ctx.Err() == nil {
		s, err := connection.Open(ctx, r.m.opts.Store, r.conn, connection.Options{Base: r.m.opts.Base, Logger: log, Tune: r.m.opts.Tune})
		if errors.Is(err, state.ErrLocked) {
			r.setPhase(phaseBusy)
			if sleep(ctx, 5*time.Second) != nil {
				return
			}
			continue
		}
		if err != nil {
			log.Warn("app: open the connection", "err", err)
			r.setPhase(phaseFailed)
			if sleep(ctx, 30*time.Second) != nil {
				return
			}
			continue
		}
		r.mu.Lock()
		r.sess, r.phase, r.conn = s, phaseStarting, s.Conn
		r.mu.Unlock()
		err = r.run(ctx, s)
		r.mu.Lock()
		r.sess, r.ready = nil, false
		r.mu.Unlock()
		s.Close()
		var old *tooOldError
		switch {
		case ctx.Err() != nil:
			return
		case errors.As(err, &old):
			// Not an error to retry: nothing syncs until the app is
			// updated (or the server lowers its minimum, asked now and
			// then).
			log.Warn("app: the server needs a newer client", "min", old.min)
			r.mu.Lock()
			r.phase, r.minVersion = phaseTooOld, old.min
			r.mu.Unlock()
			if sleep(ctx, r.m.opts.VersionCheck) != nil {
				return
			}
		case errors.Is(err, engine.ErrNeedsSignIn):
			// Until the user signs in again, which restarts the runner.
			r.setPhase(phaseNeedSignIn)
			<-ctx.Done()
			return
		case errors.Is(err, engine.ErrNoMarker):
			r.setPhase(phaseNoFolder)
			if sleep(ctx, 10*time.Second) != nil {
				return
			}
		default:
			log.Warn("app: sync stopped", "err", err)
			r.setPhase(phaseFailed)
			fails++
			if sleep(ctx, retryDelay(fails)) != nil {
				return
			}
		}
	}
}

// run lists the cloud (retrying while the server can't be reached),
// then syncs until ctx ends or the engine stops — or the server turns
// this client away as too old (*tooOldError), asked first and then every
// VersionCheck.
func (r *runner) run(ctx context.Context, s *connection.Session) error {
	if err := clientAccepted(ctx, s.Client); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		t := time.NewTicker(r.m.opts.VersionCheck)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := clientAccepted(ctx, s.Client); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	err := r.prepareAndRun(ctx, s)
	var old *tooOldError
	if cause := context.Cause(ctx); errors.As(cause, &old) {
		return cause
	}
	return err
}

// clientAccepted asks /sync/info whether the server takes this client:
// *tooOldError if not. Any other failure is the engine's to meet.
func clientAccepted(ctx context.Context, c *api.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info, err := c.Info(ctx)
	if errors.Is(err, api.ErrClientTooOld) {
		return &tooOldError{min: info.MinClientVersion}
	}
	return nil
}

func (r *runner) prepareAndRun(ctx context.Context, s *connection.Session) error {
	for fails := 1; ; fails++ {
		err := s.Engine.Prepare(ctx)
		if err == nil {
			break
		}
		if errors.Is(err, engine.ErrNeedsSignIn) || isUnauthorized(err) {
			return engine.ErrNeedsSignIn
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.m.log.Info("app: the cloud can't be listed yet", "conn", r.conn.ID, "err", err)
		if sleep(ctx, retryDelay(fails)) != nil {
			return ctx.Err()
		}
	}
	r.mu.Lock()
	r.ready, r.phase = true, phaseRunning
	r.mu.Unlock()
	return s.Engine.Run(ctx)
}

// withEngine runs fn with the open engine: ErrNotRunning without one.
func (r *runner) withEngine(fn func(*engine.Engine) error) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.sess == nil {
		return ErrNotRunning
	}
	return fn(r.sess.Engine)
}

// withReady is withEngine once the cloud is listed: engine.ErrNotReady
// before.
func (r *runner) withReady(fn func(*engine.Engine) error) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.sess == nil {
		return ErrNotRunning
	}
	if !r.ready {
		return engine.ErrNotReady
	}
	return fn(r.sess.Engine)
}

func retryDelay(fails int) time.Duration {
	d := min(time.Second<<min(fails, 5), 30*time.Second)
	return d/2 + rand.N(d/2)
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
