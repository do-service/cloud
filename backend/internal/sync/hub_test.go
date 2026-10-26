package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/share"
)

type polled struct {
	resp changesResponse
	code int
	// at is when the answer came.
	at time.Time
}

// poll starts a long poll of changes from cursor and returns where its
// answer arrives.
func (e env) poll(t *testing.T, user, cursor string, wait int) <-chan polled {
	t.Helper()
	out := make(chan polled, 1)
	go func() {
		rec := e.do(user, "/api/v1/sync/changes?cursor="+url.QueryEscape(cursor)+"&wait="+strconv.Itoa(wait))
		p := polled{code: rec.Code, at: time.Now()}
		if rec.Code == http.StatusOK {
			_ = json.Unmarshal(rec.Body.Bytes(), &p.resp)
		}
		out <- p
	}()
	return out
}

func (h *Hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.waiters)
}

// waitFor waits until n requests wait on the hub.
func (e env) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for e.hub.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d waiting, want %d", e.hub.count(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLongPoll(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice, root := e.ids["alice"], e.roots["alice"]
	shared := e.mkdir(t, "alice", root.ID, "Shared")
	e.grant(t, shared.ID, "bob", access.Read)

	// Nothing new: it waits, then answers with the change once it comes.
	cur := e.rootsOf(t, "alice").Cursor
	got := e.poll(t, "alice", cur, 30)
	e.waitFor(t, 1)
	start := time.Now()
	f := e.file(t, "alice", root.ID, "new.txt", "n")
	p := <-got
	if p.code != http.StatusOK || !slices.Equal(ids(p.resp.Changes), []string{f.ID}) {
		t.Fatalf("long poll: %d %+v", p.code, p.resp)
	}
	if d := p.at.Sub(start); d > 2*time.Second {
		t.Errorf("answered %s after the change, want under 2s", d)
	}
	t.Logf("answered %s after the change", p.at.Sub(start))

	// What it can't see doesn't end bob's wait: alice's own files, outside
	// the share, wake him at most to read and wait again.
	bobCur := e.rootsOf(t, "bob").Cursor
	start = time.Now()
	got = e.poll(t, "bob", bobCur, 2)
	e.waitFor(t, 1)
	e.file(t, "alice", root.ID, "unseen.txt", "u")
	p = <-got
	if p.code != http.StatusOK || len(p.resp.Changes) != 0 || p.resp.RootsChanged || p.at.Sub(start) < 2*time.Second {
		t.Fatalf("unshared change: %d %+v after %s", p.code, p.resp, p.at.Sub(start))
	}
	// The cursor it answers with carries on: no 410, nothing missed.
	if r := e.changesOf(t, "bob", p.resp.Cursor, 0); len(r.Changes) != 0 {
		t.Errorf("after the timeout: %+v", r)
	}

	// A share to him wakes him (his access_seq moves) with rootsChanged.
	bobCur = p.resp.Cursor
	got = e.poll(t, "bob", bobCur, 30)
	e.waitFor(t, 1)
	other := e.mkdir(t, "alice", root.ID, "Other")
	start = time.Now()
	if _, err := e.shares.Grant(ctx, alice, other.ID, share.GranteeUser, e.ids["bob"], access.Read); err != nil {
		t.Fatal(err)
	}
	if p = <-got; p.code != http.StatusOK || !p.resp.RootsChanged || p.at.Sub(start) > 2*time.Second {
		t.Fatalf("new share: %d %+v after %s", p.code, p.resp, p.at.Sub(start))
	}

	// ?wait=0 and no wait answer at once; over maxWait is refused.
	start = time.Now()
	if r := e.do("bob", "/api/v1/sync/changes?wait=0&cursor="+url.QueryEscape(p.resp.Cursor)); r.Code != http.StatusOK || time.Since(start) > time.Second {
		t.Errorf("wait=0: %d after %s", r.Code, time.Since(start))
	}
	if r := e.do("bob", "/api/v1/sync/changes?wait=56&cursor="+url.QueryEscape(p.resp.Cursor)); r.Code != http.StatusBadRequest {
		t.Errorf("wait=56: %d", r.Code)
	}

	// Shutdown ends the wait at once, with an empty answer and a cursor
	// that carries on.
	cur = e.rootsOf(t, "alice").Cursor
	got = e.poll(t, "alice", cur, 30)
	e.waitFor(t, 1)
	start = time.Now()
	e.stopHub()
	p = <-got
	if p.code != http.StatusOK || len(p.resp.Changes) != 0 || p.at.Sub(start) > time.Second {
		t.Fatalf("shutdown: %d %+v after %s", p.code, p.resp, p.at.Sub(start))
	}
	if r := e.changesOf(t, "alice", p.resp.Cursor, 0); len(r.Changes) != 0 {
		t.Errorf("after shutdown: %+v", r)
	}
	// And a poll after it doesn't wait either.
	start = time.Now()
	if p = <-e.poll(t, "alice", cur, 30); p.code != http.StatusOK || time.Since(start) > time.Second {
		t.Errorf("poll after shutdown: %d after %s", p.code, time.Since(start))
	}
}

// Waiting holds no database connection: 400 clients waiting at once
// (~the target's concurrent users) leave the pool idle, the hub alone
// queries it, once a second.
func TestLongPollPool(t *testing.T) {
	const clients = 400
	e := newEnv(t)
	root := e.roots["alice"]
	shared := e.mkdir(t, "alice", root.ID, "Shared")
	e.grant(t, shared.ID, "bob", access.Read)
	curs := map[string]string{"alice": e.rootsOf(t, "alice").Cursor, "bob": e.rootsOf(t, "bob").Cursor}

	answers := make([]<-chan polled, clients)
	for i := range answers {
		user := []string{"alice", "bob"}[i%2]
		answers[i] = e.poll(t, user, curs[user], 30)
	}
	e.waitFor(t, clients)
	if g := testutil.ToFloat64(waitersGauge); g < clients {
		t.Errorf("gauge: %v waiting, want %d", g, clients)
	}

	// Three polls of the hub's.
	peak := int32(0)
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		peak = max(peak, e.pool.Stat().AcquiredConns())
	}
	stat := e.pool.Stat()
	t.Logf("%d waiting: peak %d connections acquired, %d open of %d", clients, peak, stat.TotalConns(), stat.MaxConns())
	if peak > 1 {
		t.Errorf("%d waiting held %d connections at once, want the hub's one", clients, peak)
	}

	// One change in the shared folder answers them all, spread by the
	// jitter, every one in time.
	start := time.Now()
	f := e.file(t, "alice", shared.ID, "f.txt", "f")
	var last time.Duration
	for i, got := range answers {
		select {
		case p := <-got:
			if p.code != http.StatusOK || !slices.Equal(ids(p.resp.Changes), []string{f.ID}) {
				t.Fatalf("client %d: %d %+v", i, p.code, p.resp)
			}
			last = max(last, p.at.Sub(start))
		case <-time.After(10 * time.Second):
			t.Fatalf("client %d: no answer", i)
		}
	}
	t.Logf("all %d answered within %s of the change", clients, last)
	if last > pollInterval+maxJitter+2*time.Second {
		t.Errorf("last answer %s after the change", last)
	}
	if g := testutil.ToFloat64(waitersGauge); g != 0 {
		t.Errorf("gauge: %v waiting after all answered", g)
	}
}

// A hub that can't see a change never wakes on it: own heads and others'
// are told apart.
func TestHubMoved(t *testing.T) {
	w := &waiter{trees: map[string]int64{"me": 5, "o": 3}, user: "me", access: 2}
	for _, c := range []struct {
		heads map[string]fs.SyncHead
		want  bool
	}{
		{map[string]fs.SyncHead{"me": {TreeSeq: 5, AccessSeq: 2}, "o": {TreeSeq: 3}}, false},
		{map[string]fs.SyncHead{"me": {TreeSeq: 6, AccessSeq: 2}, "o": {TreeSeq: 3}}, true},
		{map[string]fs.SyncHead{"me": {TreeSeq: 5, AccessSeq: 3}, "o": {TreeSeq: 3}}, true},
		{map[string]fs.SyncHead{"me": {TreeSeq: 5, AccessSeq: 2}, "o": {TreeSeq: 4}}, true},
		// Someone else's access_seq is theirs, not the waiter's.
		{map[string]fs.SyncHead{"me": {TreeSeq: 5, AccessSeq: 2}, "o": {TreeSeq: 3, AccessSeq: 9}}, false},
		// No heads yet: nothing moved.
		{map[string]fs.SyncHead{}, false},
	} {
		if got := moved(w, c.heads); got != c.want {
			t.Errorf("moved(%+v) = %v, want %v", c.heads, got, c.want)
		}
	}
}
