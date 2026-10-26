package watch

import "testing"

func TestOpString(t *testing.T) {
	if got := (Create | IsFile).String(); got != "create|file" {
		t.Fatalf("String() = %q", got)
	}
	if got := Op(0).String(); got != "none" {
		t.Fatalf("String() = %q", got)
	}
}
