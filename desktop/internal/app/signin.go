package app

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/connection"
	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/state"
)

// «Добавить облако»: the sign-in in the system browser (internal/signin)
// runs in the background while the window waits on it; one at a time.

// SignInState is the window's view of the sign-in under way.
type SignInState struct {
	// State is "idle" (none), "waiting" (for the browser), "done" or
	// "error".
	State  string `json:"state"`
	Server string `json:"server,omitempty"`
	// URL is the consent page, for «если браузер не открылся».
	URL string `json:"url,omitempty"`
	// Code says what went wrong (signInCode).
	Code string `json:"code,omitempty"`
	// ConnID is the connection made, once done.
	ConnID string `json:"connId,omitempty"`
}

type signinState struct {
	mu     sync.Mutex
	st     SignInState
	cancel context.CancelFunc
}

func (s *signinState) get() SignInState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

func (s *signinState) set(f func(*SignInState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.st)
}

// ErrInvalidServer: not an address a cloud can have (signInCode
// "invalid_server").
var ErrInvalidServer = errors.New("app: not a server address")

// normalizeInput takes what the user typed: "cloud.example.com" means
// https, "localhost:8090" http — a cloud on this machine rarely has a
// certificate, and plain http is allowed for loopback only.
func normalizeInput(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw != "" && !strings.Contains(raw, "://") {
		scheme := "https://"
		if u, err := url.Parse("http://" + raw); err == nil && api.IsLoopback(u.Hostname()) {
			scheme = "http://"
		}
		raw = scheme + raw
	}
	server, err := api.NormalizeServer(raw)
	if err != nil {
		return "", ErrInvalidServer
	}
	return server, nil
}

// NewFolder is the folder «Добавить облако» shows for the cloud at raw:
// dir, or the default ~/Cloud/<host> when dir is "", as checked
// (connection.CheckFolder); err says why it can't be used. Signing in
// again to a connection already here keeps its folder.
func (m *Manager) NewFolder(raw, dir string) (connection.Folder, error) {
	server, err := normalizeInput(raw)
	if err != nil {
		return connection.Folder{}, err
	}
	if dir == "" {
		conns, err := m.reg.List()
		if err != nil {
			return connection.Folder{}, err
		}
		for _, c := range conns {
			// Most likely the same account signing in again (Setup tells
			// once the user is known).
			if c.Server == server {
				return connection.Folder{Path: c.LocalRoot, Exists: true}, nil
			}
		}
		if dir, err = connection.DefaultFolder(server); err != nil {
			return connection.Folder{}, err
		}
	}
	return connection.CheckFolder(m.opts.Base, server, "", dir)
}

// StartSignIn begins adding the cloud at raw: a sign-in in the browser,
// then the connection exactly as cloudsync login makes it (marker,
// state.db, files already in the folder adopted), and its engine. dir is
// the sync folder, "" for the default (NewFolder): checked before the
// browser opens, and again once the account is known. A sign-in already
// waiting is cancelled. The outcome is in SignIn.
func (m *Manager) StartSignIn(raw, dir string) (SignInState, error) {
	server, err := normalizeInput(raw)
	if err != nil {
		return SignInState{}, err
	}
	if _, err := m.NewFolder(server, dir); err != nil {
		return SignInState{}, err
	}
	ctx, cancel := context.WithCancel(m.ctx)
	s := &signinState{st: SignInState{State: "waiting", Server: server}, cancel: cancel}
	m.mu.Lock()
	if old := m.signin; old != nil && old.cancel != nil {
		old.cancel()
	}
	m.signin = s
	m.mu.Unlock()
	go m.signIn(ctx, s, server, dir)
	return s.get(), nil
}

func (m *Manager) signIn(ctx context.Context, s *signinState, server, dir string) {
	defer s.cancel()
	res, err := signin.Login(ctx, signin.Options{
		Server: server, OpenBrowser: m.opts.OpenBrowser,
		OnURL: func(u string) { s.set(func(st *SignInState) { st.URL = u }) },
	})
	var conn state.Connection
	if err == nil {
		var a signin.Account
		if a, err = m.opts.Store.Save(res); err == nil {
			conn, _, err = connection.Setup(ctx, m.opts.Base, a, dir)
		}
	}
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			m.log.Warn("app: sign-in", "server", server, "err", err)
		}
		s.set(func(st *SignInState) { st.State, st.Code = "error", signInCode(err) })
		return
	}
	m.mu.Lock()
	// Signed in again to a connection already here: its engine starts
	// over with the new token (it may have stopped for the old one).
	if r, ok := m.runners[conn.ID]; ok {
		r.cancel()
		m.mu.Unlock()
		<-r.done
		m.mu.Lock()
	}
	m.startLocked(conn)
	m.active = conn.ID
	m.mu.Unlock()
	s.set(func(st *SignInState) { st.State, st.ConnID = "done", conn.ID })
}

// SignIn is the sign-in under way or last finished.
func (m *Manager) SignIn() SignInState {
	m.mu.Lock()
	s := m.signin
	m.mu.Unlock()
	if s == nil {
		return SignInState{State: "idle"}
	}
	return s.get()
}

// CancelSignIn stops waiting for the browser (the «Отмена» button) and
// forgets the outcome.
func (m *Manager) CancelSignIn() {
	m.mu.Lock()
	s := m.signin
	m.signin = nil
	m.mu.Unlock()
	if s != nil {
		s.cancel()
	}
}

// signInCode is the window's code for why adding a cloud failed: a key
// of desktop.signin.errors in the web dictionary.
func signInCode(err error) string {
	var apiErr *api.APIError
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, ErrInvalidServer):
		return "invalid_server"
	case errors.Is(err, api.ErrClientTooOld):
		return "client_too_old"
	case errors.Is(err, api.ErrIncompatible), errors.As(err, &apiErr):
		// It answered, but not as a cloud of ours does.
		return "not_a_cloud"
	case errors.Is(err, signin.ErrAccessDenied):
		return "denied"
	case errors.Is(err, signin.ErrTimeout):
		return "timeout"
	case errors.Is(err, connection.ErrOtherFolder):
		return "other_folder"
	}
	if code := folderCode(err); code != "" {
		return code
	}
	return "unreachable"
}

// folderCode is the window's code for why a folder can't be a sync
// folder, "" for another error: a key of desktop.folderErrors (and of
// desktop.signin.errors) in the web dictionary.
func folderCode(err error) string {
	switch {
	case errors.Is(err, state.ErrNested):
		return "nested"
	case errors.Is(err, state.ErrLocked):
		return "busy"
	case errors.Is(err, connection.ErrNotFolder):
		return "not_a_folder"
	case errors.Is(err, connection.ErrFolderNotEmpty):
		return "folder_not_empty"
	case errors.Is(err, connection.ErrFolderTaken):
		return "folder_taken"
	case errors.Is(err, connection.ErrInsideCurrent):
		return "folder_inside"
	case errors.Is(err, connection.ErrCrossVolume):
		return "cross_volume"
	case errors.Is(err, localfs.ErrNoMarker):
		return "no_folder"
	}
	return ""
}
