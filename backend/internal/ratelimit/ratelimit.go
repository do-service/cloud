// Package ratelimit is an in-process token bucket per key (IP, username).
// In memory and per instance: the app runs as one process, and losing the
// buckets on restart only resets the limits.
package ratelimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// staleAfter is how long an idle key's bucket is kept. By then it has
// refilled anyway, so forgetting it changes nothing but memory use.
const staleAfter = 15 * time.Minute

type entry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// Limiter allows burst requests per key at once, then every per one.
type Limiter struct {
	every rate.Limit
	burst int

	mu      sync.Mutex
	entries map[string]*entry
}

func New(every time.Duration, burst int) *Limiter {
	return &Limiter{every: rate.Every(every), burst: burst, entries: map[string]*entry{}}
}

// Allow takes a token for key. When none is left it reports how long until
// one is, for the Retry-After header.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	e := l.entries[key]
	if e == nil {
		e = &entry{limiter: rate.NewLimiter(l.every, l.burst)}
		l.entries[key] = e
	}
	e.lastSeen = time.Now()
	l.mu.Unlock()

	r := e.limiter.Reserve()
	if d := r.Delay(); d > 0 {
		// Don't keep the token booked: a refused request mustn't push the
		// next allowed one further out.
		r.Cancel()
		return false, d
	}
	return true, 0
}

// Blocked reports whether key has no token left, without taking one. For
// limits that count only failures: check Blocked before the attempt, call
// Allow when it fails.
func (l *Limiter) Blocked(key string) (blocked bool, retryAfter time.Duration) {
	l.mu.Lock()
	e := l.entries[key]
	l.mu.Unlock()
	if e == nil {
		return false, 0
	}
	tokens := e.limiter.Tokens()
	if tokens >= 1 {
		return false, 0
	}
	return true, time.Duration((1 - tokens) / float64(l.every) * float64(time.Second))
}

// Run evicts idle buckets every interval until ctx is done.
func (l *Limiter) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cutoff := time.Now().Add(-staleAfter)
			l.mu.Lock()
			for k, e := range l.entries {
				if e.lastSeen.Before(cutoff) {
					delete(l.entries, k)
				}
			}
			l.mu.Unlock()
		}
	}
}
