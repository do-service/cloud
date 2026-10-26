package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// Handler is the window's /app/v1/updates, beside internal/app's
// /app/v1 (same {"code","message"} errors):
//
//	GET  /app/v1/updates               Status
//	PUT  /app/v1/updates/mode          {"mode": "off"|"notify"|"auto"}
//	POST /app/v1/updates/check         check now; Status when done
//	POST /app/v1/updates/download      download the release found
//	POST /app/v1/updates/install       install it and restart (the user's click)
//	POST /app/v1/updates/release-page  open the release's page
func (c *Controller) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app/v1/updates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, c.Status())
	})
	mux.HandleFunc("PUT /app/v1/updates/mode", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Mode Mode `json:"mode"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil || !body.Mode.Valid() {
			writeError(w, http.StatusBadRequest, "invalid_request", "mode must be off, notify or auto")
			return
		}
		if err := c.SetMode(body.Mode); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, c.Status())
	})
	mux.HandleFunc("POST /app/v1/updates/check", func(w http.ResponseWriter, r *http.Request) {
		if err := c.CheckNow(r.Context()); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c.Status())
	})
	mux.HandleFunc("POST /app/v1/updates/download", func(w http.ResponseWriter, r *http.Request) {
		if err := c.Download(); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, c.Status())
	})
	mux.HandleFunc("POST /app/v1/updates/install", func(w http.ResponseWriter, r *http.Request) {
		// Not tied to the request: the app quits on success, taking the
		// window with it.
		if err := c.Install(c.context()); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, c.Status())
	})
	mux.HandleFunc("POST /app/v1/updates/release-page", func(w http.ResponseWriter, r *http.Request) {
		if err := c.OpenReleasePage(); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func (c *Controller) context() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ctx
}

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrDisabled):
		writeError(w, http.StatusConflict, "updates_disabled", err.Error())
	case errors.Is(err, ErrManual):
		writeError(w, http.StatusConflict, "manual_update", err.Error())
	case errors.Is(err, ErrNothingToDo):
		writeError(w, http.StatusConflict, "not_ready", err.Error())
	case errors.Is(err, errors.ErrUnsupported):
		writeError(w, http.StatusNotImplemented, "unsupported", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "update_failed", err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}
