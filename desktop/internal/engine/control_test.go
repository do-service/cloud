package engine

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/state"
)

// gatedServer is the fake server with knobs: uploads of names in hold
// wait until released (or their ctx ends), the network can be cut, and
// requests are counted.
type gatedServer struct {
	*fakeServer
	hold    map[string]chan struct{}
	offline atomic.Bool
	changes atomic.Int64
	pings   atomic.Int64
	uploads atomic.Int64
	// uploadErr, when set, answers every upload.
	uploadErr atomic.Pointer[api.APIError]
}

func newGated() *gatedServer {
	return &gatedServer{fakeServer: newFakeServer(), hold: map[string]chan struct{}{}}
}

func (g *gatedServer) Changes(ctx context.Context, cursor string, limit int, wait time.Duration) (api.Changes, error) {
	g.changes.Add(1)
	if g.offline.Load() {
		return api.Changes{}, errNetwork
	}
	return g.fakeServer.Changes(ctx, cursor, limit, wait)
}

func (g *gatedServer) Upload(ctx context.Context, u api.Upload) (api.UploadResult, error) {
	g.uploads.Add(1)
	if g.offline.Load() {
		return api.UploadResult{}, errNetwork
	}
	if e := g.uploadErr.Load(); e != nil {
		return api.UploadResult{}, e
	}
	if ch := g.hold[u.Name]; ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return api.UploadResult{}, ctx.Err()
		}
	}
	return g.fakeServer.Upload(ctx, u)
}

func (g *gatedServer) Ping(ctx context.Context) error {
	g.pings.Add(1)
	if g.offline.Load() {
		return errNetwork
	}
	return nil
}

func eventually(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A large upload runs in the background: the pass that starts it and the
// passes after it go on with small files meanwhile.
func TestEngineLargeTransferDoesntBlock(t *testing.T) {
	g := newGated()
	release := make(chan struct{})
	g.hold["big.bin"] = release
	h := newHarness(t, g, "u1", func(c *Config) { c.LargeSize = 1 << 10 })
	t.Cleanup(h.close)
	h.selectAll()
	h.write("big.bin", strings.Repeat("x", 4<<10))
	h.write("small-1.txt", "1")

	done := make(chan error, 1)
	go func() {
		ctx := context.Background()
		for range 5 {
			if _, err := h.e.Pass(ctx); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("passes waited for the large upload")
	}
	if !g.has(shaOf("1")) {
		t.Fatal("the small file waited for the large one")
	}
	if st := h.e.Status(); st.Transfers != 1 {
		t.Fatalf("status: %+v", st)
	}
	// A new file while the big one still runs.
	h.write("small-2.txt", "2")
	h.e.RescanAll()
	if _, err := h.e.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !g.has(shaOf("2")) {
		t.Fatal("a pass during the large upload didn't upload a new small file")
	}
	close(release)
	h.sync()
	expectSame(t, h, g.fakeServer)
	if st := h.e.Status(); st.Transfers != 0 {
		t.Fatalf("status after: %+v", st)
	}
	if res, err := h.e.Pass(context.Background()); err != nil || res.Ops != 0 {
		t.Fatalf("extra pass: %+v, %v", res, err)
	}
}

// Pause cuts a transfer off and keeps its op for later; nothing syncs
// until Resume, also after a restart; then it all goes through once.
func TestEnginePauseResume(t *testing.T) {
	g := newGated()
	release := make(chan struct{})
	g.hold["a.txt"] = release
	h := newHarness(t, g, "u1", nil)
	t.Cleanup(h.close)
	h.selectAll()
	h.write("a.txt", "a")

	errc := make(chan error, 1)
	go func() {
		_, err := h.e.SyncOnce(context.Background())
		errc <- err
	}()
	eventually(t, "the upload to start", 5*time.Second, func() bool { return g.uploads.Load() == 1 })
	if err := h.e.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err == nil {
		t.Fatal("a paused sync finished")
	}
	if pendingOps(t, &client{harness: h}) != 1 {
		t.Fatal("the cut-off upload's op is gone")
	}
	if _, err := h.e.SyncOnce(context.Background()); !errors.Is(err, ErrPaused) {
		t.Fatalf("sync while paused: %v", err)
	}
	h.restart()
	if !h.e.Status().Paused {
		t.Fatal("the pause didn't survive a restart")
	}
	if _, err := h.e.Pass(context.Background()); !errors.Is(err, ErrPaused) {
		t.Fatalf("pass while paused: %v", err)
	}
	if g.has(shaOf("a")) {
		t.Fatal("uploaded while paused")
	}

	close(release)
	if err := h.e.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.sync()
	expectSame(t, h, g.fakeServer)
	if pendingOps(t, &client{harness: h}) != 0 {
		t.Fatal("ops left after resume")
	}
}

// Run follows the flag another process sets (cloudsync pause / resume).
func TestEngineRunFollowsPauseFlag(t *testing.T) {
	g := newGated()
	h := newHarness(t, g, "u1", nil)
	t.Cleanup(h.close)
	h.selectAll()
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- h.e.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-runDone; err != nil {
			t.Error(err)
		}
	}()
	setFlag := func(p bool) {
		// As cloudsync pause does: the database only, not the engine.
		if err := h.db.Update(context.Background(), func(tx *state.Tx) error { return tx.SetPaused(p) }); err != nil {
			t.Fatal(err)
		}
	}
	h.write("one.txt", "1")
	eventually(t, "one.txt uploaded", 10*time.Second, func() bool { return g.has(shaOf("1")) })

	setFlag(true)
	eventually(t, "the engine to pause", 5*time.Second, func() bool { return h.e.Status().Paused })
	h.write("two.txt", "2")
	h.e.RescanAll()
	polls := g.changes.Load()
	time.Sleep(1500 * time.Millisecond)
	if g.has(shaOf("2")) {
		t.Fatal("uploaded while paused")
	}
	if n := g.changes.Load() - polls; n > 1 {
		t.Fatalf("%d polls of the server while paused", n)
	}

	setFlag(false)
	eventually(t, "two.txt uploaded after resume", 10*time.Second, func() bool { return g.has(shaOf("2")) })
}

// Offline: passes back off (no busy loop, pings instead of whole passes),
// the status says so and a problem tells the CLI; back online, sync
// resumes within a backoff step and the problem goes.
func TestEngineOfflineBackoff(t *testing.T) {
	g := newGated()
	h := newHarness(t, g, "u1", nil)
	t.Cleanup(h.close)
	h.selectAll()
	h.sync()

	g.offline.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- h.e.Run(ctx) }()
	defer func() {
		cancel()
		<-runDone
	}()
	eventually(t, "offline status", 5*time.Second, func() bool { return h.e.Status().Offline })
	hasOffline := func() bool {
		for _, p := range h.problems() {
			if p.Kind == ProblemOffline {
				return true
			}
		}
		return false
	}
	if !hasOffline() {
		t.Fatal("no offline problem")
	}
	start := g.changes.Load() + g.pings.Load()
	time.Sleep(3 * time.Second)
	if n := g.changes.Load() + g.pings.Load() - start; n > 6 {
		t.Fatalf("%d requests in 3 s offline: not backing off", n)
	}
	if st := h.e.Status(); st.NextTry.IsZero() || st.OfflineSince.IsZero() {
		t.Fatalf("status: %+v", st)
	}

	h.write("back.txt", "b")
	g.offline.Store(false)
	// A local change wakes the wait: no need to sit out the backoff.
	eventually(t, "sync after the network is back", 10*time.Second, func() bool { return g.has(shaOf("b")) })
	eventually(t, "online status", 5*time.Second, func() bool { return !h.e.Status().Offline && !hasOffline() })
}

// 507 (quota) is a problem, not a retry loop: the upload is tried once
// and then left alone for its long backoff. 423 keeps the op and retries
// it, a problem only after LockRetryFor.
func TestEngineQuotaAndLocked(t *testing.T) {
	t.Run("quota", func(t *testing.T) {
		g := newGated()
		g.uploadErr.Store(&api.APIError{Status: http.StatusInsufficientStorage, Code: api.CodeQuotaExceeded})
		h := newHarness(t, g, "u1", nil)
		t.Cleanup(h.close)
		h.selectAll()
		h.write("big.txt", "q")
		h.sync()
		h.sync()
		if n := g.uploads.Load(); n != 1 {
			t.Fatalf("%d upload attempts, want 1", n)
		}
		ps := h.problems()
		if len(ps) != 1 || ps[0].Kind != ProblemQuota || ps[0].RelPath != "big.txt" {
			t.Fatalf("problems: %+v", ps)
		}
	})
	t.Run("locked", func(t *testing.T) {
		g := newGated()
		g.uploadErr.Store(&api.APIError{Status: http.StatusLocked, Code: api.CodeLocked})
		h := newHarness(t, g, "u1", func(c *Config) {
			c.LockBackoff, c.LockRetryFor = 20*time.Millisecond, 200*time.Millisecond
		})
		t.Cleanup(h.close)
		h.selectAll()
		h.write("doc.txt", "l")
		h.sync()
		if pendingOps(t, &client{harness: h}) != 1 || len(h.problems()) != 0 {
			t.Fatalf("after a 423: %d ops, problems %+v", pendingOps(t, &client{harness: h}), h.problems())
		}
		deadline := time.Now().Add(5 * time.Second)
		for !hasKind(h.problems(), ProblemLocked) {
			if time.Now().After(deadline) {
				t.Fatalf("no lock problem after LockRetryFor; %d attempts", g.uploads.Load())
			}
			time.Sleep(30 * time.Millisecond)
			h.sync()
		}
		if n := g.uploads.Load(); n < 2 {
			t.Fatalf("%d attempts: a 423 wasn't retried", n)
		}
		g.uploadErr.Store(nil)
		h.e.mu.Lock()
		h.e.backoff = map[string]*backoff{}
		h.e.mu.Unlock()
		_ = h.db.Update(context.Background(), func(tx *state.Tx) error {
			ops, _ := tx.Ops()
			for _, o := range ops {
				_ = tx.RetryOp(o.ID)
			}
			return nil
		})
		h.sync()
		expectSame(t, h, g.fakeServer)
	})
}

func hasKind(ps []state.Problem, kind string) bool {
	for _, p := range ps {
		if p.Kind == kind {
			return true
		}
	}
	return false
}

// The idle wait long-polls with Config.Wait, and a local change doesn't
// wait for the poll to end; a server that ignores wait (answers at once)
// is polled no more often than PollInterval.
func TestEngineLongPoll(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		lp := &longPollServer{gatedServer: newGated(), hold: true}
		h := newHarness(t, lp, "u1", func(c *Config) { c.Wait = 30 * time.Second })
		t.Cleanup(h.close)
		h.selectAll()
		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- h.e.Run(ctx) }()
		defer func() {
			cancel()
			<-runDone
		}()
		eventually(t, "a long poll", 5*time.Second, func() bool { return lp.waiting.Load() })
		if w := time.Duration(lp.wait.Load()); w != 30*time.Second {
			t.Fatalf("long poll with wait %s", w)
		}
		start := time.Now()
		h.write("now.txt", "n")
		eventually(t, "the upload during a long poll", 5*time.Second, func() bool { return lp.has(shaOf("n")) })
		if d := time.Since(start); d > 4*time.Second {
			t.Fatalf("a local change waited %s for the long poll", d)
		}
	})
	t.Run("ignored", func(t *testing.T) {
		lp := &longPollServer{gatedServer: newGated()}
		h := newHarness(t, lp, "u1", func(c *Config) { c.PollInterval = 300 * time.Millisecond })
		t.Cleanup(h.close)
		h.selectAll()
		h.sync()
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		before := lp.changes.Load()
		if err := h.e.Run(ctx); err != nil {
			t.Fatal(err)
		}
		// ~5 polls in 1.5 s; a pass right after each poll asks nothing more.
		if n := lp.changes.Load() - before; n > 8 {
			t.Fatalf("%d requests for changes in 1.5 s with a server that ignores wait", n)
		}
	})
}

// longPollServer holds a Changes with a wait until its ctx ends (when
// hold), like a server with nothing new; else answers at once.
type longPollServer struct {
	*gatedServer
	hold    bool
	waiting atomic.Bool
	wait    atomic.Int64
}

func (s *longPollServer) Changes(ctx context.Context, cursor string, limit int, wait time.Duration) (api.Changes, error) {
	if wait > 0 && s.hold {
		s.wait.Store(int64(wait))
		s.waiting.Store(true)
		defer s.waiting.Store(false)
		select {
		case <-ctx.Done():
			return api.Changes{}, ctx.Err()
		case <-time.After(wait):
		}
	}
	return s.gatedServer.Changes(ctx, cursor, limit, wait)
}
