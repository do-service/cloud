package update

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// fakeEngine plays Wails' updater: next is what Check finds (nil: up to
// date), and every call is counted.
type fakeEngine struct {
	mu                         sync.Mutex
	next                       *updater.Release
	checkErr, dlErr            error
	checks, downloads, restart int
}

func (f *fakeEngine) Check(context.Context) (*updater.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.next, f.checkErr
}

func (f *fakeEngine) DownloadAndInstall(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads++
	return f.dlErr
}

func (f *fakeEngine) Restart(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restart++
	return nil
}

func (f *fakeEngine) counts() (checks, downloads, restarts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks, f.downloads, f.restart
}

// clock is a settable Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestController(t *testing.T, eng Engine, mode Mode, install Install, enabled bool) (*Controller, *clock) {
	t.Helper()
	base := t.TempDir()
	if mode != DefaultMode {
		if err := SaveMode(base, mode); err != nil {
			t.Fatal(err)
		}
	}
	clk := &clock{t: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	c := NewController(Options{Engine: eng, Base: base, Version: "1.0.0", Enabled: enabled, Install: install, Now: clk.Now, Logger: slog.New(slog.DiscardHandler)})
	c.poll = 5 * time.Millisecond
	return c, clk
}

func run(t *testing.T, c *Controller) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// settle lets Run go round its loop many times.
func settle() { time.Sleep(60 * time.Millisecond) }

func TestOffNeverChecks(t *testing.T) {
	eng := &fakeEngine{next: &updater.Release{Version: "1.1.0"}}
	c, clk := newTestController(t, eng, ModeOff, InstallSelf, true)
	run(t, c)
	settle()
	clk.add(30 * 24 * time.Hour)
	settle()
	if n, _, _ := eng.counts(); n != 0 {
		t.Fatalf("checks with updates off: %d", n)
	}
	// «Проверить сейчас» still checks, and tells; nothing downloads.
	if err := c.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); st.State != StateAvailable || st.Release == nil || st.Release.Version != "1.1.0" || st.LastCheck == nil {
		t.Fatalf("after checking now: %+v", st)
	}
	settle()
	if _, d, _ := eng.counts(); d != 0 {
		t.Fatalf("downloads with updates off: %d", d)
	}

	// Turning checks on starts the schedule, counted from that check.
	if err := c.SetMode(ModeNotify); err != nil {
		t.Fatal(err)
	}
	settle()
	if n, _, _ := eng.counts(); n != 1 {
		t.Fatalf("checked again right after checking: %d", n)
	}
	clk.add(DefaultInterval)
	eventually(t, "a check after turning them on", func() bool { n, _, _ := eng.counts(); return n == 2 })
	if m, _ := LoadMode(c.opts.Base); m != ModeNotify {
		t.Fatalf("mode saved: %s", m)
	}
}

func TestScheduleEveryInterval(t *testing.T) {
	eng := &fakeEngine{}
	c, clk := newTestController(t, eng, ModeNotify, InstallSelf, true)
	run(t, c)
	eventually(t, "the check on start", func() bool { n, _, _ := eng.counts(); return n == 1 })
	if st := c.Status(); st.State != StateUpToDate || st.Release != nil {
		t.Fatalf("up to date: %+v", st)
	}
	clk.add(DefaultInterval - time.Minute)
	settle()
	if n, _, _ := eng.counts(); n != 1 {
		t.Fatalf("checked before the interval: %d", n)
	}
	clk.add(time.Minute)
	eventually(t, "the check after the interval", func() bool { n, _, _ := eng.counts(); return n == 2 })

	// A failed check is tried again sooner.
	eng.mu.Lock()
	eng.checkErr = errors.New("offline")
	eng.mu.Unlock()
	clk.add(DefaultInterval)
	eventually(t, "the failing check", func() bool { n, _, _ := eng.counts(); return n == 3 })
	if st := c.Status(); st.State != StateError || st.ErrorStage != StageCheck {
		t.Fatalf("after a failed check: %+v", st)
	}
	clk.add(retryAfterError)
	eventually(t, "the retry", func() bool { n, _, _ := eng.counts(); return n == 4 })

	// Turned off: no more checks however long.
	if err := c.SetMode(ModeOff); err != nil {
		t.Fatal(err)
	}
	clk.add(10 * DefaultInterval)
	settle()
	if n, _, _ := eng.counts(); n != 4 {
		t.Fatalf("checks after turning off: %d", n)
	}
}

func TestNotifyDownloadsOnClick(t *testing.T) {
	eng := &fakeEngine{next: &updater.Release{Version: "1.1.0"}}
	c, _ := newTestController(t, eng, ModeNotify, InstallSelf, true)
	run(t, c)
	eventually(t, "the release found", func() bool { return c.Status().State == StateAvailable })
	settle()
	if _, d, _ := eng.counts(); d != 0 {
		t.Fatal("notify mode downloaded by itself")
	}
	if err := c.Install(context.Background()); !errors.Is(err, ErrNothingToDo) {
		t.Fatalf("install before downloading: %v", err)
	}
	if err := c.Download(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "downloaded", func() bool { return c.Status().State == StateReady })
	if _, _, r := eng.counts(); r != 0 {
		t.Fatal("installed without the user's click")
	}
	if err := c.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, r := eng.counts(); r != 1 || c.Status().State != StateInstalling {
		t.Fatalf("install: %d restarts, %+v", r, c.Status())
	}
}

func TestAutoDownloadsThenAsks(t *testing.T) {
	eng := &fakeEngine{next: &updater.Release{Version: "1.1.0"}}
	c, clk := newTestController(t, eng, ModeAuto, InstallSelf, true)
	run(t, c)
	eventually(t, "downloaded by itself", func() bool { return c.Status().State == StateReady })
	st := c.Status()
	if st.Release == nil || st.Release.Version != "1.1.0" || st.Release.Page != "https://github.com/do-service/cloud/releases/tag/v1.1.0" {
		t.Fatalf("release: %+v", st.Release)
	}
	// Staged: later checks don't download it again; nothing installs
	// until Install.
	clk.add(3 * DefaultInterval)
	settle()
	if n, d, r := eng.counts(); n != 1 || d != 1 || r != 0 {
		t.Fatalf("checks %d, downloads %d, restarts %d", n, d, r)
	}
}

func TestAutoDownloadFailure(t *testing.T) {
	eng := &fakeEngine{next: &updater.Release{Version: "1.1.0"}, dlErr: errors.New("signature did not verify")}
	c, _ := newTestController(t, eng, ModeAuto, InstallSelf, true)
	run(t, c)
	eventually(t, "the failed download", func() bool { return c.Status().State == StateError })
	if st := c.Status(); st.ErrorStage != StageDownload || st.Release == nil {
		t.Fatalf("after a failed download: %+v", st)
	}
	if err := c.Install(context.Background()); !errors.Is(err, ErrNothingToDo) {
		t.Fatalf("install of a failed download: %v", err)
	}
	// «Скачать» tries again.
	eng.mu.Lock()
	eng.dlErr = nil
	eng.mu.Unlock()
	if err := c.Download(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "downloaded again", func() bool { return c.Status().State == StateReady })
}

func TestManualInstallOnlyTells(t *testing.T) {
	eng := &fakeEngine{next: &updater.Release{Version: "1.1.0"}}
	c, _ := newTestController(t, eng, ModeAuto, InstallManual, true)
	run(t, c)
	eventually(t, "the release found", func() bool { return c.Status().State == StateAvailable })
	settle()
	if _, d, _ := eng.counts(); d != 0 {
		t.Fatal("a package install downloaded the update")
	}
	if err := c.Download(); !errors.Is(err, ErrManual) {
		t.Fatalf("download: %v", err)
	}
	var opened string
	c.opts.OpenBrowser = func(u string) error { opened = u; return nil }
	if err := c.OpenReleasePage(); err != nil || opened != ReleasePage("1.1.0") {
		t.Fatalf("release page: %q %v", opened, err)
	}
}

func TestDevelopmentBuild(t *testing.T) {
	eng := &fakeEngine{next: &updater.Release{Version: "1.1.0"}}
	c, _ := newTestController(t, eng, ModeAuto, InstallSelf, false)
	run(t, c)
	settle()
	if err := c.CheckNow(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("check now: %v", err)
	}
	if n, _, _ := eng.counts(); n != 0 {
		t.Fatalf("a development build checked: %d", n)
	}
	if st := c.Status(); st.Enabled || st.State != StateIdle {
		t.Fatalf("status: %+v", st)
	}
}

func TestHandler(t *testing.T) {
	eng := &fakeEngine{next: &updater.Release{Version: "1.1.0"}}
	c, _ := newTestController(t, eng, ModeNotify, InstallSelf, true)
	h := c.Handler()
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "wails://localhost"+path, strings.NewReader(body)))
		return rec
	}
	if rec := do(http.MethodGet, "/app/v1/updates", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mode":"notify"`) || !strings.Contains(rec.Body.String(), `"version":"1.0.0"`) {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	if rec := do(http.MethodPut, "/app/v1/updates/mode", `{"mode":"sometimes"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: %d", rec.Code)
	}
	if rec := do(http.MethodPut, "/app/v1/updates/mode", `{"mode":"off"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mode":"off"`) {
		t.Fatalf("set mode: %d %s", rec.Code, rec.Body)
	}
	if rec := do(http.MethodPost, "/app/v1/updates/install", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"not_ready"`) {
		t.Fatalf("install with nothing: %d %s", rec.Code, rec.Body)
	}
	if rec := do(http.MethodPost, "/app/v1/updates/check", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"available"`) {
		t.Fatalf("check: %d %s", rec.Code, rec.Body)
	}
	if rec := do(http.MethodPost, "/app/v1/updates/download", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("download: %d %s", rec.Code, rec.Body)
	}
	eventually(t, "downloaded", func() bool { return c.Status().State == StateReady })
	if rec := do(http.MethodPost, "/app/v1/updates/install", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("install: %d %s", rec.Code, rec.Body)
	}
	if _, _, r := eng.counts(); r != 1 {
		t.Fatalf("restarts: %d", r)
	}
}
