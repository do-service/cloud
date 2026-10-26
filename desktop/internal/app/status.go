package app

import (
	"context"
	"net/url"

	"github.com/do-service/cloud/desktop/internal/engine"
	"github.com/do-service/cloud/desktop/internal/state"
)

// Status is a connection's state as the window and the tray show it.
type Status string

const (
	StatusStarting   Status = "starting"
	StatusSynced     Status = "synced"
	StatusSyncing    Status = "syncing"
	StatusPaused     Status = "paused"
	StatusOffline    Status = "offline"
	StatusError      Status = "error"
	StatusNeedSignIn Status = "needs_signin"
	StatusNoFolder   Status = "no_folder"
	// StatusBusy: another process (cloudsync, a second app) syncs it.
	StatusBusy Status = "busy"
	// StatusMoving: its sync folder is being moved (Manager.MoveFolder).
	StatusMoving Status = "moving"
	// StatusTooOld: the server needs a newer client (ConnInfo.MinVersion);
	// nothing syncs until the app is updated.
	StatusTooOld Status = "too_old"
)

// ConnInfo is a connection in the window's list.
type ConnInfo struct {
	ID     string `json:"id"`
	Server string `json:"server"`
	// Host is the server without the scheme, as the list shows it.
	Host   string `json:"host"`
	User   string `json:"user"`
	Folder string `json:"folder"`
	Status Status `json:"status"`
	// Pending ops in the journal and Problems recorded (E14-4 lists them).
	Pending  int `json:"pending"`
	Problems int `json:"problems"`
	// MinVersion is the client version the server needs (StatusTooOld).
	MinVersion string `json:"minVersion,omitempty"`
}

// informational problems: listed (E14-4), but nothing is wrong with sync.
var informational = map[string]bool{engine.ProblemConflict: true, engine.ProblemRescued: true, engine.ProblemOffline: true}

// Connections lists the connections in the order they were added, with
// their status.
func (m *Manager) Connections(ctx context.Context) []ConnInfo {
	rs := m.runnersInOrder()
	out := make([]ConnInfo, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.info(ctx))
	}
	return out
}

// ViewSettings asks the window to show «Настройки» (the tray's item).
const ViewSettings = "settings"

// RequestView asks the window to show view: the next
// GET /app/v1/connections carries it once.
func (m *Manager) RequestView(view string) {
	m.mu.Lock()
	m.view = view
	m.mu.Unlock()
}

func (m *Manager) takeView() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.view
	m.view = ""
	return v
}

// Active is the connection the window shows, "" with none.
func (m *Manager) Active() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

func (r *runner) info(ctx context.Context) ConnInfo {
	c := r.connection()
	host := c.Server
	if u, err := url.Parse(c.Server); err == nil {
		host = u.Host
	}
	ci := ConnInfo{ID: c.ID, Server: c.Server, Host: host, User: c.UserName, Folder: c.LocalRoot}
	r.mu.RLock()
	defer r.mu.RUnlock()
	switch r.phase {
	case phaseBusy:
		ci.Status = StatusBusy
		return ci
	case phaseNeedSignIn:
		ci.Status = StatusNeedSignIn
		return ci
	case phaseNoFolder:
		ci.Status = StatusNoFolder
		return ci
	case phaseFailed:
		ci.Status = StatusError
		return ci
	case phaseMoving:
		ci.Status = StatusMoving
		return ci
	case phaseTooOld:
		ci.Status, ci.MinVersion = StatusTooOld, r.minVersion
		return ci
	}
	if r.sess == nil {
		ci.Status = StatusStarting
		return ci
	}
	st := r.sess.Engine.Status()
	var ops []state.Op
	var probs []state.Problem
	if err := r.sess.DB.View(ctx, func(tx *state.Tx) error {
		var err error
		if ops, err = tx.Ops(); err != nil {
			return err
		}
		probs, err = tx.Problems()
		return err
	}); err != nil {
		ci.Status = StatusError
		return ci
	}
	ci.Pending, ci.Problems = len(ops), len(probs)
	failing := false
	for _, p := range probs {
		switch {
		case p.Kind == engine.ProblemNeedsSignIn:
			ci.Status = StatusNeedSignIn
			return ci
		case p.Kind == engine.ProblemNoMarker:
			ci.Status = StatusNoFolder
			return ci
		case !informational[p.Kind]:
			failing = true
		}
	}
	switch {
	case st.Paused:
		ci.Status = StatusPaused
	case st.Offline:
		ci.Status = StatusOffline
	case failing:
		ci.Status = StatusError
	case len(ops) > 0 || st.Transfers > 0:
		ci.Status = StatusSyncing
	case !r.ready:
		// Listing the cloud the first time: that is syncing too, to the
		// user.
		ci.Status = StatusSyncing
	default:
		ci.Status = StatusSynced
	}
	return ci
}

// Overall is the tray's state over all connections: the worst of them.
// Paused only when every connection is; none at all reads as synced
// (nothing to do).
func Overall(conns []ConnInfo) Status {
	if len(conns) == 0 {
		return StatusSynced
	}
	rank := func(s Status) int {
		switch s {
		case StatusError, StatusNeedSignIn, StatusNoFolder, StatusTooOld:
			return 5
		case StatusOffline:
			return 4
		case StatusSyncing, StatusStarting, StatusMoving:
			return 3
		case StatusSynced, StatusBusy:
			return 2
		}
		return 1 // paused
	}
	worst := conns[0].Status
	for _, c := range conns[1:] {
		if rank(c.Status) > rank(worst) {
			worst = c.Status
		}
	}
	switch worst {
	case StatusNeedSignIn, StatusNoFolder, StatusTooOld:
		return StatusError
	case StatusStarting, StatusMoving:
		return StatusSyncing
	case StatusBusy:
		return StatusSynced
	}
	return worst
}
