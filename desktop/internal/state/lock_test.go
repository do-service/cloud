package state

import (
	"errors"
	"testing"
)

func TestLockConnection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", "")
	l, err := LockConnection("c1")
	if err != nil {
		t.Fatal(err)
	}
	// flock is per open file, so a second open in this process stands in
	// for another process.
	if _, err := LockConnection("c1"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	other, err := LockConnection("c2")
	if err != nil {
		t.Fatalf("another connection: %v", err)
	}
	other.Unlock()
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	again, err := LockConnection("c1")
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	again.Unlock()
}
