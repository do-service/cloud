package sync

import (
	"context"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	gosync "sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/do-service/cloud/backend/internal/fs"
)

// The long poll of changes (E11-4, docs/desktop-client.md). A waiting
// request holds no database connection: it sleeps on a channel, and one
// loop per process reads the heads of the trees its waiters follow once a
// second and wakes those whose trees moved on. The table is the signal, so
// a write on any API node wakes waiters on every node; LISTEN/NOTIFY would
// take a global lock on every commit, and SSE gets buffered by proxies.

const (
	// pollInterval is how often the hub reads the heads: the delay a
	// change waits for at most before its waiters wake.
	pollInterval = time.Second
	// maxJitter spreads the waiters one change wakes over this long, so a
	// folder shared with hundreds doesn't send them all to the database
	// in the same instant; jitterStep is the spread per waiter woken, so
	// a change few wait on is answered at once.
	maxJitter  = 2 * time.Second
	jitterStep = 5 * time.Millisecond
)

var waitersGauge = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "cloud_sync_waiters",
	Help: "Long polls of /api/v1/sync/changes waiting for a change.",
})

// Hub wakes long polls when the trees they follow change.
type Hub struct {
	// Heads reads the heads of the given users' trees (fs.SyncHeads).
	Heads func(ctx context.Context, users []string) (map[string]fs.SyncHead, error)
	// Interval is pollInterval but in tests.
	Interval time.Duration

	mu      gosync.Mutex
	waiters map[*waiter]struct{}
	// done is closed once Run returns: the server is shutting down.
	done     chan struct{}
	stopOnce gosync.Once
}

func NewHub(files *fs.Service) *Hub {
	return &Hub{Heads: files.SyncHeads, Interval: pollInterval}
}

// waiter is one waiting request: what its client has of each tree it
// follows, and whose access_seq it has.
type waiter struct {
	trees  map[string]int64
	user   string
	access int64
	// wake gets how long to hold back before answering.
	wake chan time.Duration
}

func (h *Hub) init() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.waiters == nil {
		h.waiters = map[*waiter]struct{}{}
		h.done = make(chan struct{})
	}
}

// Run polls until ctx is done, then releases every waiter, so long polls
// end at once on shutdown instead of holding it up.
func (h *Hub) Run(ctx context.Context) {
	h.init()
	defer h.stopOnce.Do(func() { close(h.done) })
	interval := h.Interval
	if interval <= 0 {
		interval = pollInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.poll(ctx)
		}
	}
}

func (h *Hub) poll(ctx context.Context) {
	h.mu.Lock()
	users := map[string]bool{}
	for w := range h.waiters {
		users[w.user] = true
		for u := range w.trees {
			users[u] = true
		}
	}
	h.mu.Unlock()
	if len(users) == 0 {
		return
	}
	heads, err := h.Heads(ctx, slices.Collect(maps.Keys(users)))
	if err != nil {
		if ctx.Err() == nil {
			slog.ErrorContext(ctx, "sync: read heads for long polls", "err", err)
		}
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var woken []*waiter
	for w := range h.waiters {
		if moved(w, heads) {
			woken = append(woken, w)
			delete(h.waiters, w)
		}
	}
	spread := min(maxJitter, time.Duration(len(woken))*jitterStep)
	for _, w := range woken {
		w.wake <- time.Duration(rand.Int64N(int64(spread) + 1))
	}
	waitersGauge.Sub(float64(len(woken)))
}

// moved reports whether a tree w follows, or its access, moved past what
// its client has.
func moved(w *waiter, heads map[string]fs.SyncHead) bool {
	if heads[w.user].AccessSeq > w.access {
		return true
	}
	for u, seq := range w.trees {
		if heads[u].TreeSeq > seq {
			return true
		}
	}
	return false
}

// wait blocks until a tree in view moves on, until, or ctx is done. It
// reports whether to read the journal again: false on a timeout, a gone
// client, or shutdown, when the request answers with what it has.
func (h *Hub) wait(ctx context.Context, userID string, view fs.SyncView, until time.Time) bool {
	h.init()
	w := &waiter{trees: map[string]int64{userID: view.Own.TreeSeq}, user: userID, access: view.Own.AccessSeq, wake: make(chan time.Duration, 1)}
	for _, owner := range view.Owners() {
		w.trees[owner] = view.Heads[owner].TreeSeq
	}
	h.mu.Lock()
	h.waiters[w] = struct{}{}
	waitersGauge.Inc()
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if _, ok := h.waiters[w]; ok {
			delete(h.waiters, w)
			waitersGauge.Dec()
		}
		h.mu.Unlock()
	}()

	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()
	select {
	case delay := <-w.wake:
		hold := time.NewTimer(delay)
		defer hold.Stop()
		select {
		case <-hold.C:
			return true
		case <-timer.C:
			return true // read once more: the change is there
		case <-ctx.Done():
		case <-h.done:
		}
	case <-timer.C:
	case <-ctx.Done():
	case <-h.done:
	}
	return false
}
