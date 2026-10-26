package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	l := New(time.Hour, 2)
	for i := range 2 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d within burst refused", i)
		}
	}
	ok, retry := l.Allow("a")
	if ok || retry <= 0 || retry > time.Hour {
		t.Fatalf("over burst: ok %v, retry %s", ok, retry)
	}
	// Refusals don't book tokens: the wait doesn't grow with each one.
	if _, again := l.Allow("a"); again > retry {
		t.Fatalf("retry grew from %s to %s", retry, again)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("keys share a bucket")
	}
}

func TestBlocked(t *testing.T) {
	l := New(time.Hour, 2)
	if blocked, _ := l.Blocked("a"); blocked {
		t.Fatal("unknown key blocked")
	}
	l.Allow("a")
	if blocked, _ := l.Blocked("a"); blocked {
		t.Fatal("blocked with a token left")
	}
	// Blocked doesn't take tokens: asking twice leaves the one left.
	l.Blocked("a")
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("Blocked took a token")
	}
	blocked, retry := l.Blocked("a")
	if !blocked || retry <= 0 || retry > time.Hour {
		t.Fatalf("exhausted: blocked %v, retry %s", blocked, retry)
	}
}
