package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPasswordHash(t *testing.T) {
	ctx := context.Background()
	h, err := hashPassword(ctx, "correct horse")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("encoding %q", h)
	}
	if again, _ := hashPassword(ctx, "correct horse"); again == h {
		t.Fatal("two hashes of one password are equal: salt is not random")
	}
	for pw, want := range map[string]bool{"correct horse": true, "correct horsE": false, "": false} {
		if ok, err := verifyPassword(ctx, h, pw); err != nil || ok != want {
			t.Errorf("verify %q: %v, %v", pw, ok, err)
		}
	}
}

// Parameters live in the hash: a hash made with other costs still verifies
// after the defaults change.
func TestPasswordHashOtherParams(t *testing.T) {
	ctx := context.Background()
	old := defaultArgon
	defaultArgon = argonParams{memoryKiB: 8 * 1024, time: 1, threads: 1, keyLen: 16}
	h, err := hashPassword(ctx, "pw")
	defaultArgon = old
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := verifyPassword(ctx, h, "pw"); err != nil || !ok {
		t.Fatalf("verify with stored params: %v, %v", ok, err)
	}
}

func TestPasswordHashMalformed(t *testing.T) {
	for _, h := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$a2V5",
		"$argon2id$v=18$m=65536,t=3,p=4$c2FsdA$a2V5",
		"$argon2id$v=19$m=65536,t=3,p=4$!!$a2V5",
		// Costs an attacker with database write access could use to make
		// every login allocate 4 GiB.
		"$argon2id$v=19$m=4194304,t=3,p=4$c2FsdA$a2V5",
	} {
		if _, err := verifyPassword(context.Background(), h, "pw"); !errors.Is(err, errMalformedHash) {
			t.Errorf("%q: err %v", h, err)
		}
	}
}

func TestPasswordHashHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Fill the slots so the call has to wait for one.
	for range cap(hashSlots) {
		hashSlots <- struct{}{}
	}
	defer func() {
		for range cap(hashSlots) {
			<-hashSlots
		}
	}()
	if _, err := hashPassword(ctx, "pw"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}
