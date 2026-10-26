package signin

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"sync"
	"time"

	"github.com/do-service/cloud/desktop/internal/api"
)

type outcome struct {
	token api.DeviceToken
	err   error
}

// callback serves the loopback redirect: the first request to /cb ends
// the sign-in, whatever it says, and the browser gets a page saying how
// it went. The code is exchanged before the page is written, so the page
// tells the final result, not just "received".
type callback struct {
	state    string
	exchange func(ctx context.Context, code string) (api.DeviceToken, error)
	done     chan outcome

	mu       sync.Mutex
	finished bool
}

// exchangeTimeout bounds the code exchange; the code lives 120 s anyway.
const exchangeTimeout = 30 * time.Second

func (c *callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/cb" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		writePage(w, http.StatusGone, false, "Этот вход уже завершён. Если нужно, начните вход заново в приложении.")
		return
	}
	c.finished = true

	q := r.URL.Query()
	var o outcome
	switch {
	case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(c.state)) != 1:
		o.err = ErrStateMismatch
	case q.Get("error") == "access_denied":
		o.err = ErrAccessDenied
	case q.Get("error") != "":
		o.err = fmt.Errorf("signin: the server answered %q", q.Get("error"))
	case q.Get("code") == "":
		o.err = errors.New("signin: the browser came back without a code")
	default:
		// Not the request's context: a browser closing the tab mustn't
		// throw away a code that is single-use.
		ctx, cancel := context.WithTimeout(context.Background(), exchangeTimeout)
		o.token, o.err = c.exchange(ctx, q.Get("code"))
		cancel()
	}
	c.done <- o
	if o.err == nil {
		writePage(w, http.StatusOK, true, "Готово, можно вернуться в приложение.")
		return
	}
	writePage(w, http.StatusBadRequest, false, failureText(o.err))
}

// close makes later requests get "finished": Login has stopped waiting.
func (c *callback) close() {
	c.mu.Lock()
	c.finished = true
	c.mu.Unlock()
}

func failureText(err error) string {
	switch {
	case errors.Is(err, ErrAccessDenied):
		return "Вход не выполнен: доступ не разрешён. Можно закрыть эту вкладку."
	case errors.Is(err, ErrStateMismatch):
		return "Вход не выполнен: этот ответ не от текущего входа. Начните вход заново в приложении."
	case errors.Is(err, api.ErrInvalidGrant):
		return "Вход не выполнен: код входа устарел или уже использован. Начните вход заново в приложении."
	}
	return "Вход не выполнен. Вернитесь в приложение и попробуйте ещё раз."
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .OK}}Готово{{else}}Вход не выполнен{{end}} — Cloud</title>
<style>
:root { color-scheme: light dark; }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; background: Canvas; color: CanvasText; }
main { max-width: 28rem; padding: 2rem 1.5rem; text-align: center; }
.mark { font-size: 2.5rem; }
</style>
</head>
<body><main><div class="mark" aria-hidden="true">{{if .OK}}✓{{else}}✕{{end}}</div><p>{{.Text}}</p></main></body>
</html>
`))

func writePage(w http.ResponseWriter, status int, ok bool, text string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	// The code is in this page's URL: no referrer, no scripts, no frames.
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, struct {
		OK   bool
		Text string
	}{ok, text})
}
