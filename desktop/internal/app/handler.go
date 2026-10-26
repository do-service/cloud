package app

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/do-service/cloud/desktop/internal/connection"
	"github.com/do-service/cloud/desktop/internal/engine"
	"github.com/do-service/cloud/desktop/internal/state"
)

// Handler is the window's own API under /app/v1, next to the /api proxy
// in the app's asset handler. The window calls it with fetch like the
// server's API, rather than through Wails' generated bindings: no
// wails3 code generation or JS runtime to keep in step with Wails, the
// same {"code","message"} errors as the server, and tests without cgo.
// It is served only to the app's own WebView (the asset handler is no
// network listener).
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app/v1/connections", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": m.Connections(r.Context()), "active": m.Active()})
	})
	mux.HandleFunc("PUT /app/v1/active", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if err := m.SetActive(body.ID); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /app/v1/connections/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Remove(r.Context(), r.PathValue("id"), r.URL.Query().Get("force") == "1"); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /app/v1/connections/{id}/open-folder", func(w http.ResponseWriter, r *http.Request) {
		if err := m.OpenFolder(r.PathValue("id")); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	for path, paused := range map[string]bool{"pause": true, "resume": false} {
		mux.HandleFunc("POST /app/v1/connections/{id}/"+path, func(w http.ResponseWriter, r *http.Request) {
			if err := m.SetPaused(r.Context(), r.PathValue("id"), paused); err != nil {
				fail(w, r, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("POST /app/v1/connections/{id}/marks", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs []string `json:"ids"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		run, ok := m.runner(r.PathValue("id"))
		if !ok {
			fail(w, r, state.ErrNotFound)
			return
		}
		var marks map[string]engine.Mark
		err := run.withReady(func(e *engine.Engine) error {
			var err error
			marks, err = e.Marks(r.Context(), body.IDs)
			return err
		})
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": marks})
	})
	mux.HandleFunc("GET /app/v1/connections/{id}/usage", func(w http.ResponseWriter, r *http.Request) {
		run, ok := m.runner(r.PathValue("id"))
		if !ok {
			fail(w, r, state.ErrNotFound)
			return
		}
		var u engine.Usage
		if err := run.withReady(func(e *engine.Engine) error {
			var err error
			u, err = e.Usage(r.Context())
			return err
		}); err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, u)
	})
	// PUT checks a node, DELETE unchecks it; the engine is woken to sync
	// at once rather than after its long poll.
	mux.HandleFunc("PUT /app/v1/connections/{id}/selection/{node}", func(w http.ResponseWriter, r *http.Request) {
		run, ok := m.runner(r.PathValue("id"))
		if !ok {
			fail(w, r, state.ErrNotFound)
			return
		}
		var need int64
		err := run.withReady(func(e *engine.Engine) error {
			var err error
			need, err = e.SelectNode(r.Context(), r.PathValue("node"))
			if err == nil {
				e.Wake()
			}
			return err
		})
		if errors.Is(err, engine.ErrNoSpace) {
			writeJSON(w, http.StatusInsufficientStorage, map[string]any{"code": "no_space", "message": "not enough free disk space", "need": need})
			return
		}
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"need": need})
	})
	mux.HandleFunc("DELETE /app/v1/connections/{id}/selection/{node}", func(w http.ResponseWriter, r *http.Request) {
		run, ok := m.runner(r.PathValue("id"))
		if !ok {
			fail(w, r, state.ErrNotFound)
			return
		}
		if err := run.withReady(func(e *engine.Engine) error {
			err := e.UnselectNode(r.Context(), r.PathValue("node"))
			if err == nil {
				e.Wake()
			}
			return err
		}); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /app/v1/signin", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.SignIn())
	})
	mux.HandleFunc("POST /app/v1/signin", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Server string `json:"server"`
			Folder string `json:"folder"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		st, err := m.StartSignIn(body.Server, body.Folder)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, st)
	})
	// The folder «Добавить облако» would connect: ?server= and ?path=
	// ("" the default), with code saying why it can't be one.
	mux.HandleFunc("GET /app/v1/new-folder", func(w http.ResponseWriter, r *http.Request) {
		f, err := m.NewFolder(r.URL.Query().Get("server"), r.URL.Query().Get("path"))
		writeFolder(w, r, f, err)
	})
	mux.HandleFunc("POST /app/v1/pick-folder", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Start string `json:"start"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		path, err := m.PickFolder(body.Start)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"path": path})
	})
	// «Сменить папку»: GET checks ?path=, PUT moves the folder there.
	mux.HandleFunc("GET /app/v1/connections/{id}/folder", func(w http.ResponseWriter, r *http.Request) {
		run, ok := m.runner(r.PathValue("id"))
		if !ok {
			fail(w, r, state.ErrNotFound)
			return
		}
		f, err := connection.CheckMoveTarget(m.opts.Base, run.connection(), r.URL.Query().Get("path"))
		writeFolder(w, r, f, err)
	})
	mux.HandleFunc("PUT /app/v1/connections/{id}/folder", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		c, err := m.MoveFolder(r.Context(), r.PathValue("id"), body.Path)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"folder": c.LocalRoot})
	})
	// What the folder held when connected and the cloud has unchecked at
	// its top (engine.Adoptable); POST checks them all.
	mux.HandleFunc("GET /app/v1/connections/{id}/adoptable", func(w http.ResponseWriter, r *http.Request) {
		run, ok := m.runner(r.PathValue("id"))
		if !ok {
			fail(w, r, state.ErrNotFound)
			return
		}
		var ids []string
		if err := run.withReady(func(e *engine.Engine) error {
			var err error
			ids, err = e.Adoptable(r.Context())
			return err
		}); err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": append([]string{}, ids...)})
	})
	mux.HandleFunc("POST /app/v1/connections/{id}/adopt", func(w http.ResponseWriter, r *http.Request) {
		run, ok := m.runner(r.PathValue("id"))
		if !ok {
			fail(w, r, state.ErrNotFound)
			return
		}
		var need int64
		err := run.withReady(func(e *engine.Engine) error {
			ids, err := e.Adoptable(r.Context())
			if err != nil {
				return err
			}
			defer e.Wake()
			for _, id := range ids {
				n, err := e.SelectNode(r.Context(), id)
				need += n
				if err != nil {
					return err
				}
			}
			return nil
		})
		if errors.Is(err, engine.ErrNoSpace) {
			writeJSON(w, http.StatusInsufficientStorage, map[string]any{"code": "no_space", "message": "not enough free disk space", "need": need})
			return
		}
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"need": need})
	})
	mux.HandleFunc("DELETE /app/v1/signin", func(w http.ResponseWriter, r *http.Request) {
		m.CancelSignIn()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return false
	}
	return true
}

// writeFolder answers a folder check: the folder, and code when it can't
// be used (anything but a refusal fails the request).
func writeFolder(w http.ResponseWriter, r *http.Request, f connection.Folder, err error) {
	code := ""
	if err != nil {
		if code = folderCode(err); code == "" || code == "busy" {
			fail(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, struct {
		connection.Folder
		Code string `json:"code,omitempty"`
	}{f, code})
}

// fail answers an error with the code the window has a text for.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	if code := folderCode(err); code != "" && code != "busy" {
		writeError(w, http.StatusConflict, code, err.Error())
		return
	}
	switch {
	case errors.Is(err, state.ErrNotFound), errors.Is(err, engine.ErrNotInCloud):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrInvalidServer):
		writeError(w, http.StatusBadRequest, "invalid_server", err.Error())
	case errors.Is(err, engine.ErrNotReady), errors.Is(err, ErrNotRunning):
		writeError(w, http.StatusConflict, "not_ready", err.Error())
	case errors.Is(err, state.ErrLocked):
		writeError(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, errors.ErrUnsupported):
		writeError(w, http.StatusNotImplemented, "unsupported", err.Error())
	default:
		// Mostly the server unreachable (a logout that can't revoke).
		slog.WarnContext(r.Context(), "app: request failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusBadGateway, "network", err.Error())
	}
}
