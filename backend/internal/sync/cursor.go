package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"

	"github.com/do-service/cloud/backend/internal/fs"
)

// cursor is how far a client has followed the journals: opaque to it,
// base64 of this JSON.
type cursor struct {
	V int `json:"v"`
	// Own is the last seq of the caller's own tree they have.
	Own int64 `json:"own"`
	// A is their access_seq: when it moves, so did what they see of
	// others' trees.
	A int64 `json:"a"`
	// S is the last seq they have of each tree with something shared
	// with them, by owner.
	S map[string]int64 `json:"s"`
	// R fingerprints the shares to them, rights included: one granted,
	// revoked or changed tells the client to list its roots again. The
	// share changes bump access_seq too (fs.TouchAccess), which is what
	// wakes a long poll; R also catches what moves no seq, such as a
	// cursor from before a database restore.
	R string `json:"r"`
}

const cursorVersion = 1

// maxCursor bounds what decode reads: a cursor names one tree per owner
// sharing with the caller, at most a few thousand.
const maxCursor = 1 << 18

var (
	errBadCursor = errors.New("sync: malformed cursor")
	uuidPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	fingerprintP = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// cursorAt is the cursor of a client that has everything up to view.
func cursorAt(view fs.SyncView) cursor {
	c := cursor{V: cursorVersion, Own: view.Own.TreeSeq, A: view.Own.AccessSeq, S: map[string]int64{}, R: fingerprint(view)}
	for _, owner := range view.Owners() {
		c.S[owner] = view.Heads[owner].TreeSeq
	}
	return c
}

// fingerprint sums up view's shares: which nodes, with which rights.
func fingerprint(view fs.SyncView) string {
	var lines []string
	for owner, nodes := range view.Shared {
		for node, p := range nodes {
			lines = append(lines, fmt.Sprintf("%s/%s:%d", owner, node, p))
		}
	}
	slices.Sort(lines)
	sum := sha256.New()
	for _, l := range lines {
		_, _ = io.WriteString(sum, l+"\n")
	}
	return hex.EncodeToString(sum.Sum(nil)[:8])
}

func (c cursor) encode() string {
	b, err := json.Marshal(c)
	if err != nil {
		// Ints and strings only: it can't fail.
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor reads a cursor and rejects anything encode wouldn't make.
func decodeCursor(s string) (cursor, error) {
	if s == "" || len(s) > maxCursor {
		return cursor{}, errBadCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, errBadCursor
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c cursor
	if err := dec.Decode(&c); err != nil || dec.Decode(new(json.RawMessage)) != io.EOF {
		return cursor{}, errBadCursor
	}
	if c.V != cursorVersion || c.Own < 0 || c.A < 0 || !fingerprintP.MatchString(c.R) {
		return cursor{}, errBadCursor
	}
	for owner, seq := range c.S {
		if !uuidPattern.MatchString(owner) || seq < 0 {
			return cursor{}, errBadCursor
		}
	}
	if c.S == nil {
		c.S = map[string]int64{}
	}
	return c, nil
}
