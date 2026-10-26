// Package storage is the content store under the fs service: bytes by key,
// nothing about users, permissions or the tree's metadata. Implementations
// are localfs (a readable directory tree) and, later, S3.
package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

var (
	ErrNotExist = errors.New("storage: key does not exist")
	ErrExist    = errors.New("storage: key already exists")
	// ErrInvalidKey is a programming error in the caller: keys are built
	// by the fs service, never taken from a client verbatim.
	ErrInvalidKey   = errors.New("storage: invalid key")
	ErrSizeMismatch = errors.New("storage: content size differs from the declared size")
	ErrInvalidRange = errors.New("storage: range outside the content")
)

// Layout tells the fs service where a file's current content lives.
type Layout int

const (
	// LayoutTree: current content sits at a path mirroring the user's tree
	// (files/{user_id}/Docs/a.txt) and is moved along with it.
	LayoutTree Layout = iota + 1
	// LayoutFlat: every content is its own object (blobs/{blob_id}); moving
	// a node never touches storage.
	LayoutFlat
)

// Range selects Length bytes from Offset; a negative Length reads to the end.
type Range struct {
	Offset int64
	Length int64
}

type PutResult struct {
	Size   int64
	SHA256 [32]byte
}

type Info struct {
	Size    int64
	ModTime time.Time
	IsDir   bool
}

// Backend stores content by key. Keys are slash-separated relative paths
// (see ValidateKey). Every write is atomic: readers see the old content or
// the new one, never a partial file. Missing parent "directories" of a key
// are created as needed.
type Backend interface {
	Layout() Layout
	// Put stores r under key, replacing what was there. size is the
	// declared length, or -1 if unknown; a mismatch stores nothing and
	// fails with ErrSizeMismatch. The hash is computed while writing so the
	// caller doesn't read the content back.
	Put(ctx context.Context, key string, r io.Reader, size int64) (PutResult, error)
	// Get opens key for reading; rng nil means the whole content.
	Get(ctx context.Context, key string, rng *Range) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (Info, error)
	// Mkdir creates key as a directory with its parents; no-op if it
	// exists, and for LayoutFlat backends.
	Mkdir(ctx context.Context, key string) error
	// Move renames a file or, for LayoutTree, a whole directory. It never
	// replaces: an existing to fails with ErrExist.
	Move(ctx context.Context, from, to string) error
	// Copy duplicates a file (not a directory), replacing to.
	Copy(ctx context.Context, from, to string) error
	// Delete removes a file or a directory with everything below it. A
	// missing key is not an error, so cleanup can be retried.
	Delete(ctx context.Context, key string) error
}

// ValidateKey rejects keys that could escape the store or alias another
// key: empty or absolute keys, empty, "." and ".." segments, NUL bytes.
func ValidateKey(key string) error {
	if key == "" || strings.ContainsRune(key, 0) {
		return ErrInvalidKey
	}
	for seg := range strings.SplitSeq(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ErrInvalidKey
		}
	}
	return nil
}
