// Package storagetest is the behaviour every storage.Backend must share,
// run against each implementation so localfs and S3 can't drift apart.
package storagetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/do-service/cloud/backend/internal/storage"
)

// Run runs the contract against fresh, empty backends from newBackend.
func Run(t *testing.T, newBackend func(t *testing.T) storage.Backend) {
	tests := []struct {
		name string
		fn   func(*testing.T, storage.Backend)
	}{
		{"PutGet", testPutGet},
		{"PutReplaces", testPutReplaces},
		{"PutSizeMismatch", testPutSizeMismatch},
		{"PutCancelled", testPutCancelled},
		{"GetRange", testGetRange},
		{"Missing", testMissing},
		{"MoveFile", testMoveFile},
		{"MoveDir", testMoveDir},
		{"Copy", testCopy},
		{"Delete", testDelete},
		{"InvalidKeys", testInvalidKeys},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, newBackend(t)) })
	}
}

// key exercises non-ASCII names and nested parents in every test.
const key = "files/u1/Документы/Отчёт за квартал 🎉.txt"

func put(t *testing.T, b storage.Backend, key, content string) storage.PutResult {
	t.Helper()
	res, err := b.Put(context.Background(), key, strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	return res
}

func read(t *testing.T, b storage.Backend, key string, rng *storage.Range) string {
	t.Helper()
	rc, err := b.Get(context.Background(), key, rng)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return string(data)
}

func mustNotExist(t *testing.T, b storage.Backend, key string) {
	t.Helper()
	if _, err := b.Stat(context.Background(), key); !errors.Is(err, storage.ErrNotExist) {
		t.Fatalf("stat %s: err %v, want ErrNotExist", key, err)
	}
}

func testPutGet(t *testing.T, b storage.Backend) {
	res := put(t, b, key, "hello")
	if res.Size != 5 || res.SHA256 != sha256.Sum256([]byte("hello")) {
		t.Fatalf("put result %+v", res)
	}
	if got := read(t, b, key, nil); got != "hello" {
		t.Fatalf("content %q", got)
	}
	info, err := b.Stat(context.Background(), key)
	if err != nil || info.Size != 5 || info.IsDir {
		t.Fatalf("stat %+v, %v", info, err)
	}

	// Unknown size is accepted and measured.
	res, err = b.Put(context.Background(), "files/u1/unknown", strings.NewReader("abc"), -1)
	if err != nil || res.Size != 3 {
		t.Fatalf("put unknown size: %+v, %v", res, err)
	}
	// Empty files are files too.
	if res := put(t, b, "files/u1/empty", ""); res.Size != 0 {
		t.Fatalf("empty put %+v", res)
	}
}

func testPutReplaces(t *testing.T, b storage.Backend) {
	put(t, b, key, "old content")
	put(t, b, key, "new")
	if got := read(t, b, key, nil); got != "new" {
		t.Fatalf("content %q", got)
	}
}

func testPutSizeMismatch(t *testing.T, b storage.Backend) {
	put(t, b, key, "keep me")
	for _, size := range []int64{3, 100} {
		_, err := b.Put(context.Background(), key, strings.NewReader("hello"), size)
		if !errors.Is(err, storage.ErrSizeMismatch) {
			t.Fatalf("size %d: err %v, want ErrSizeMismatch", size, err)
		}
	}
	// A failed put leaves the previous content alone.
	if got := read(t, b, key, nil); got != "keep me" {
		t.Fatalf("content %q", got)
	}
}

func testPutCancelled(t *testing.T, b storage.Backend) {
	ctx, cancel := context.WithCancel(context.Background())
	// Cancelled mid-stream: the first chunk is read, then the client goes.
	r := io.MultiReader(strings.NewReader("first"), readerFunc(func([]byte) (int, error) {
		cancel()
		return 0, context.Canceled
	}))
	if _, err := b.Put(ctx, key, r, -1); err == nil {
		t.Fatal("put succeeded after cancel")
	}
	mustNotExist(t, b, key)
}

func testGetRange(t *testing.T, b storage.Backend) {
	put(t, b, key, "0123456789")
	cases := []struct {
		rng  storage.Range
		want string
	}{
		{storage.Range{Offset: 2, Length: 3}, "234"},
		{storage.Range{Offset: 7, Length: -1}, "789"},
		{storage.Range{Offset: 8, Length: 100}, "89"},
		{storage.Range{Offset: 10, Length: -1}, ""},
	}
	for _, tc := range cases {
		if got := read(t, b, key, &tc.rng); got != tc.want {
			t.Errorf("range %+v: %q, want %q", tc.rng, got, tc.want)
		}
	}
	if _, err := b.Get(context.Background(), key, &storage.Range{Offset: 11, Length: -1}); !errors.Is(err, storage.ErrInvalidRange) {
		t.Fatalf("offset past end: err %v", err)
	}
}

func testMissing(t *testing.T, b storage.Backend) {
	ctx := context.Background()
	if _, err := b.Get(ctx, key, nil); !errors.Is(err, storage.ErrNotExist) {
		t.Errorf("get: %v", err)
	}
	mustNotExist(t, b, key)
	if err := b.Move(ctx, key, "files/u1/b"); !errors.Is(err, storage.ErrNotExist) {
		t.Errorf("move: %v", err)
	}
	if err := b.Copy(ctx, key, "files/u1/b"); !errors.Is(err, storage.ErrNotExist) {
		t.Errorf("copy: %v", err)
	}
}

func testMoveFile(t *testing.T, b storage.Backend) {
	ctx := context.Background()
	put(t, b, key, "data")
	dst := "versions/n1/v1"
	if err := b.Move(ctx, key, dst); err != nil {
		t.Fatalf("move: %v", err)
	}
	mustNotExist(t, b, key)
	if got := read(t, b, dst, nil); got != "data" {
		t.Fatalf("moved content %q", got)
	}

	// Move never replaces.
	put(t, b, key, "other")
	if err := b.Move(ctx, key, dst); !errors.Is(err, storage.ErrExist) {
		t.Fatalf("move onto existing: %v", err)
	}
	if got := read(t, b, dst, nil); got != "data" {
		t.Fatalf("target clobbered: %q", got)
	}
}

func testMoveDir(t *testing.T, b storage.Backend) {
	if b.Layout() != storage.LayoutTree {
		t.Skip("directories exist only in a tree layout")
	}
	ctx := context.Background()
	put(t, b, "files/u1/Docs/a.txt", "a")
	put(t, b, "files/u1/Docs/sub/b.txt", "b")
	if err := b.Mkdir(ctx, "files/u1/Docs/empty"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := b.Mkdir(ctx, "files/u1/Docs/empty"); err != nil {
		t.Fatalf("mkdir again: %v", err)
	}

	if err := b.Move(ctx, "files/u1/Docs", "trash/n1"); err != nil {
		t.Fatalf("move dir: %v", err)
	}
	mustNotExist(t, b, "files/u1/Docs")
	if got := read(t, b, "trash/n1/sub/b.txt", nil); got != "b" {
		t.Fatalf("moved subtree content %q", got)
	}
	if info, err := b.Stat(ctx, "trash/n1/empty"); err != nil || !info.IsDir {
		t.Fatalf("empty dir after move: %+v, %v", info, err)
	}
	if _, err := b.Get(ctx, "trash/n1", nil); !errors.Is(err, storage.ErrNotExist) {
		t.Fatalf("get on a directory: %v", err)
	}
}

func testCopy(t *testing.T, b storage.Backend) {
	ctx := context.Background()
	put(t, b, key, "original")
	dst := "files/u2/copy.txt"
	put(t, b, dst, "to be replaced")
	if err := b.Copy(ctx, key, dst); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if got := read(t, b, dst, nil); got != "original" {
		t.Fatalf("copy content %q", got)
	}
	// Copies are independent.
	put(t, b, key, "changed")
	if got := read(t, b, dst, nil); got != "original" {
		t.Fatalf("copy follows the source: %q", got)
	}
}

func testDelete(t *testing.T, b storage.Backend) {
	ctx := context.Background()
	put(t, b, key, "x")
	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	mustNotExist(t, b, key)
	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	if b.Layout() == storage.LayoutTree {
		put(t, b, "trash/n1/a/b/c.txt", "x")
		if err := b.Delete(ctx, "trash/n1"); err != nil {
			t.Fatalf("delete subtree: %v", err)
		}
		mustNotExist(t, b, "trash/n1")
	}
}

func testInvalidKeys(t *testing.T, b storage.Backend) {
	ctx := context.Background()
	put(t, b, key, "x")
	for _, bad := range []string{"", "/etc/passwd", "files/../../etc", "files/./a", "files//a", "files/a/", "a\x00b", ".."} {
		calls := map[string]error{
			"put":    func() error { _, err := b.Put(ctx, bad, bytes.NewReader(nil), 0); return err }(),
			"get":    func() error { _, err := b.Get(ctx, bad, nil); return err }(),
			"stat":   func() error { _, err := b.Stat(ctx, bad); return err }(),
			"mkdir":  b.Mkdir(ctx, bad),
			"delete": b.Delete(ctx, bad),
			"move":   b.Move(ctx, key, bad),
			"copy":   b.Copy(ctx, key, bad),
		}
		for op, err := range calls {
			if !errors.Is(err, storage.ErrInvalidKey) {
				t.Errorf("%s %q: err %v, want ErrInvalidKey", op, bad, err)
			}
		}
	}
	// None of the above touched the valid key.
	if got := read(t, b, key, nil); got != "x" {
		t.Fatalf("content %q", got)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
