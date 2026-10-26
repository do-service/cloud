package update

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// Engine is the part of Wails' *updater.Updater the controller drives:
// Check finds a newer release, DownloadAndInstall downloads, verifies
// and stages it, Restart hands it to the swap helper and quits the app.
type Engine interface {
	Check(ctx context.Context) (*updater.Release, error)
	DownloadAndInstall(ctx context.Context) error
	Restart(ctx context.Context) error
}

// State is where the controller is.
type State string

const (
	StateIdle        State = "idle"
	StateChecking    State = "checking"
	StateUpToDate    State = "up_to_date"
	StateAvailable   State = "available"
	StateDownloading State = "downloading"
	// StateReady: downloaded and verified; installing waits for the
	// user's click.
	StateReady      State = "ready"
	StateInstalling State = "installing"
	StateError      State = "error"
)

// Stage is the step an error came from.
type Stage string

const (
	StageCheck    Stage = "check"
	StageDownload Stage = "download" // download, signature check or unpacking
	StageInstall  Stage = "install"
)

const (
	// DefaultInterval between checks.
	DefaultInterval = 6 * time.Hour
	// retryAfterError: a check that failed (offline at boot, say) is tried
	// again sooner than the next regular one.
	retryAfterError = 30 * time.Minute
	checkTimeout    = time.Minute
)

// Options configure a Controller.
type Options struct {
	Engine Engine
	// Base is the state directory with the settings file.
	Base string
	// Version is the running client's (api.Version).
	Version string
	// Enabled is false for development builds (IsRelease): no checks.
	Enabled bool
	Install Install
	// Interval between checks, DefaultInterval when 0.
	Interval time.Duration
	// OpenBrowser opens the release page.
	OpenBrowser func(url string) error
	Logger      *slog.Logger
	// Now is the clock (tests).
	Now func() time.Time
}

// Release is the newer release found.
type Release struct {
	Version string `json:"version"`
	// Page is the release's page: notes, and packages to update by hand.
	Page string `json:"page"`
}

// Progress of a download.
type Progress struct {
	Written int64 `json:"written"`
	Total   int64 `json:"total"`
}

// Status is the window's and the tray's view of updates.
type Status struct {
	Mode    Mode   `json:"mode"`
	Version string `json:"version"`
	// Enabled is false in a development build.
	Enabled bool    `json:"enabled"`
	Install Install `json:"install"`
	State   State   `json:"state"`
	// LastCheck is when the last check ended, nil before the first.
	LastCheck *time.Time `json:"lastCheck,omitempty"`
	// ErrorStage says what failed when State is StateError.
	ErrorStage Stage     `json:"errorStage,omitempty"`
	Release    *Release  `json:"release,omitempty"`
	Progress   *Progress `json:"progress,omitempty"`
}

// Controller runs update checks by the user's mode and tells the window
// and the tray what it found. Nothing is installed without Install, which
// only the user's click calls.
type Controller struct {
	opts Options
	log  *slog.Logger
	wake chan struct{}
	// poll is how often Run reads the clock (tests shorten it).
	poll time.Duration
	// checkMu and dlMu keep one check and one download at a time.
	checkMu sync.Mutex
	dlMu    sync.Mutex

	mu        sync.Mutex
	mode      Mode
	state     State
	stage     Stage
	lastCheck time.Time
	next      time.Time
	release   *Release
	progress  *Progress
	ctx       context.Context
}

// NewController reads the mode from the settings file.
func NewController(opts Options) *Controller {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	mode, err := LoadMode(opts.Base)
	if err != nil {
		opts.Logger.Warn("update: read the settings", "err", err)
	}
	return &Controller{opts: opts, log: opts.Logger, wake: make(chan struct{}, 1), poll: time.Minute, mode: mode, state: StateIdle, ctx: context.Background()}
}

// Run checks on start and every Interval while the mode isn't ModeOff,
// until ctx ends. A development build doesn't check.
func (c *Controller) Run(ctx context.Context) {
	c.mu.Lock()
	c.ctx = ctx
	c.mu.Unlock()
	if !c.opts.Enabled {
		return
	}
	// The clock is read every minute rather than trusting one long timer:
	// a timer doesn't count the time a laptop sleeps.
	tick := time.NewTicker(c.poll)
	defer tick.Stop()
	for {
		c.mu.Lock()
		due := c.mode != ModeOff && !c.opts.Now().Before(c.next)
		c.mu.Unlock()
		if due {
			c.check(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-tick.C:
		}
	}
}

// Status is the current state.
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Status{Mode: c.mode, Version: c.opts.Version, Enabled: c.opts.Enabled, Install: c.opts.Install, State: c.state, Release: c.release, Progress: c.progress}
	if c.state == StateError {
		s.ErrorStage = c.stage
	}
	if !c.lastCheck.IsZero() {
		t := c.lastCheck
		s.LastCheck = &t
	}
	return s
}

// SetMode saves the user's choice; a mode that checks starts a check if
// one is due.
func (c *Controller) SetMode(m Mode) error {
	if err := SaveMode(c.opts.Base, m); err != nil {
		return err
	}
	c.mu.Lock()
	c.mode = m
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}

// ErrDisabled: a development build doesn't update.
var ErrDisabled = errors.New("update: updates are off in a development build")

// CheckNow checks at once, whatever the mode, and returns when done.
func (c *Controller) CheckNow(ctx context.Context) error {
	if !c.opts.Enabled {
		return ErrDisabled
	}
	c.check(ctx)
	return nil
}

func (c *Controller) check(ctx context.Context) {
	if !c.checkMu.TryLock() {
		return // one is running; its result is the answer
	}
	defer c.checkMu.Unlock()
	c.mu.Lock()
	switch c.state {
	case StateDownloading, StateReady, StateInstalling:
		// Something is staged or on its way; a check would only find it
		// again.
		c.next = c.opts.Now().Add(c.opts.Interval)
		c.mu.Unlock()
		return
	}
	c.state = StateChecking
	c.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	rel, err := c.opts.Engine.Check(cctx)
	cancel()

	c.mu.Lock()
	now := c.opts.Now()
	c.lastCheck = now
	switch {
	case err != nil:
		c.log.Warn("update: check", "err", err)
		c.state, c.stage, c.release = StateError, StageCheck, nil
		c.next = now.Add(min(retryAfterError, c.opts.Interval))
		c.mu.Unlock()
		return
	case rel == nil:
		c.state, c.release = StateUpToDate, nil
		c.next = now.Add(c.opts.Interval)
		c.mu.Unlock()
		return
	}
	c.next = now.Add(c.opts.Interval)
	c.state = StateAvailable
	c.release = &Release{Version: rel.Version, Page: ReleasePage(rel.Version)}
	auto := c.mode == ModeAuto && c.opts.Install == InstallSelf
	c.mu.Unlock()
	c.log.Info("update: a new version", "version", rel.Version, "auto", auto)
	if auto {
		go c.download(ctx)
	}
}

// ErrNothingToDo: no release to download, or none downloaded to install.
var ErrNothingToDo = errors.New("update: nothing to do in this state")

// ErrManual: this copy can't replace itself; the release page it is.
var ErrManual = errors.New("update: this installation is updated by hand")

// Download downloads the release found in the background (ModeNotify's
// «Скачать»).
func (c *Controller) Download() error {
	c.mu.Lock()
	state, ctx, found := c.state, c.ctx, c.release != nil
	c.mu.Unlock()
	if c.opts.Install != InstallSelf {
		return ErrManual
	}
	// A failed download is tried again from here too.
	if state != StateAvailable && !(state == StateError && found) {
		return ErrNothingToDo
	}
	go c.download(ctx)
	return nil
}

func (c *Controller) download(ctx context.Context) {
	if !c.dlMu.TryLock() {
		return
	}
	defer c.dlMu.Unlock()
	c.mu.Lock()
	c.state, c.progress = StateDownloading, nil
	c.mu.Unlock()
	err := c.opts.Engine.DownloadAndInstall(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.progress = nil
	if err != nil {
		c.log.Warn("update: download", "err", err)
		c.state, c.stage = StateError, StageDownload
		return
	}
	c.state = StateReady
	c.log.Info("update: downloaded and verified", "version", c.release.Version)
}

// SetProgress records a download's progress (the updater's progress
// events).
func (c *Controller) SetProgress(written, total int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == StateDownloading {
		c.progress = &Progress{Written: written, Total: total}
	}
}

// Install installs the downloaded release and restarts the app into it:
// the updater starts its helper and quits the app; the helper swaps the
// app once it has exited and starts the new one. Only the user's click
// calls it.
func (c *Controller) Install(ctx context.Context) error {
	c.mu.Lock()
	if c.state != StateReady {
		c.mu.Unlock()
		return ErrNothingToDo
	}
	c.state = StateInstalling
	c.mu.Unlock()
	if err := c.opts.Engine.Restart(ctx); err != nil {
		c.log.Warn("update: install", "err", err)
		c.mu.Lock()
		c.state, c.stage = StateError, StageInstall
		c.mu.Unlock()
		return err
	}
	return nil
}

// OpenReleasePage opens the found release's page in the browser.
func (c *Controller) OpenReleasePage() error {
	c.mu.Lock()
	rel := c.release
	c.mu.Unlock()
	if rel == nil {
		return ErrNothingToDo
	}
	if c.opts.OpenBrowser == nil {
		return errors.ErrUnsupported
	}
	return c.opts.OpenBrowser(rel.Page)
}
