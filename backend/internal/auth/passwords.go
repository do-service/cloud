package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// argonParams are Argon2id's cost parameters. They are stored in every
// hash, so raising them later only affects new hashes; old ones keep
// verifying with the parameters they were made with.
type argonParams struct {
	memoryKiB uint32
	time      uint32
	threads   uint8
	keyLen    uint32
}

// defaultArgon is RFC 9106's second recommended option (64 MiB, 3 passes),
// the one meant for memory-constrained servers.
var defaultArgon = argonParams{memoryKiB: 64 * 1024, time: 3, threads: 4, keyLen: 32}

const saltLen = 16

// hashSlots caps concurrent hashing: each run allocates memoryKiB, so a
// burst of 400 logins would otherwise need 25 GiB at once. Past the cap
// requests queue (bounded by their context) instead of exhausting memory.
var hashSlots = make(chan struct{}, 4)

var errMalformedHash = errors.New("auth: malformed password hash")

func hashPassword(ctx context.Context, password string) (string, error) {
	var salt [saltLen]byte
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(salt[:])
	key, err := deriveKey(ctx, password, salt[:], defaultArgon)
	if err != nil {
		return "", err
	}
	p := defaultArgon
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.memoryKiB, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt[:]), base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifyPassword reports whether password matches encoded, a PHC string as
// made by hashPassword.
func verifyPassword(ctx context.Context, encoded, password string) (bool, error) {
	p, salt, want, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	got, err := deriveKey(ctx, password, salt, p)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func deriveKey(ctx context.Context, password string, salt []byte, p argonParams) ([]byte, error) {
	select {
	case hashSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-hashSlots }()
	return argon2.IDKey([]byte(password), salt, p.time, p.memoryKiB, p.threads, p.keyLen), nil
}

func parseHash(encoded string) (argonParams, []byte, []byte, error) {
	// "", "argon2id", "v=19", "m=…,t=…,p=…", salt, key
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, errMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return argonParams{}, nil, nil, errMalformedHash
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memoryKiB, &p.time, &p.threads); err != nil {
		return argonParams{}, nil, nil, errMalformedHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, errMalformedHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return argonParams{}, nil, nil, errMalformedHash
	}
	// A stored hash with absurd costs would let whoever wrote it make
	// every login allocate gigabytes; refuse instead of trusting it.
	if p.memoryKiB == 0 || p.memoryKiB > 1<<20 || p.time == 0 || p.time > 16 || p.threads == 0 {
		return argonParams{}, nil, nil, errMalformedHash
	}
	p.keyLen = uint32(len(key))
	return p, salt, key, nil
}

// dummyHash is verified against when the user doesn't exist or has no
// password, so a failed login takes as long either way and response time
// doesn't reveal which usernames exist.
var dummyHash = sync.OnceValue(func() string {
	h, err := hashPassword(context.Background(), "dummy password for timing")
	if err != nil {
		panic(err) // only a cancelled context fails, and this one isn't
	}
	return h
})

// HashPassword hashes a password for storing outside this package (a
// public link's), with the same Argon2id settings and concurrency cap.
func HashPassword(ctx context.Context, password string) (string, error) {
	return hashPassword(ctx, password)
}

// VerifyPassword checks password against a HashPassword result.
func VerifyPassword(ctx context.Context, encoded, password string) (bool, error) {
	return verifyPassword(ctx, encoded, password)
}
