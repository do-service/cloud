package fs

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestCopy(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "docs")
	sub := e.mkdir(t, docs.ID, "sub")
	f := e.file(t, sub.ID, "a.txt", "hello")

	// A file: the same content and hash, a new blob.
	c, err := e.svc.Copy(ctx, e.user, f.ID, docs.ID, "", CopyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.read(t, c.ID, nil); got != "hello" || !bytes.Equal(c.ContentHash, f.ContentHash) || c.BlobID() == f.BlobID() || c.Size != 5 {
		t.Fatalf("copy %+v: %q", c, got)
	}
	// Taken: fails, or with Rename gets a free name — files and folders.
	if _, err := e.svc.Copy(ctx, e.user, f.ID, docs.ID, "", CopyOptions{}); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("taken name: %v", err)
	}
	c2, err := e.svc.Copy(ctx, e.user, f.ID, docs.ID, "", CopyOptions{Rename: true})
	if err != nil || c2.Name != "a (2).txt" {
		t.Fatalf("renamed copy: %q, %v", c2.Name, err)
	}
	d, err := e.svc.Copy(ctx, e.user, sub.ID, docs.ID, "", CopyOptions{Rename: true})
	if err != nil || d.Name != "sub (2)" {
		t.Fatalf("renamed folder copy: %q, %v", d.Name, err)
	}
	inner, err := e.svc.Lookup(ctx, e.user, d.ID, []string{"a.txt"})
	if err != nil || e.read(t, inner.ID, nil) != "hello" {
		t.Fatalf("copied folder's file: %v", err)
	}
	e.checkSizes(t)

	// How big a copy would be, counted only as far as asked.
	// docs holds 6 nodes now: sub, sub/a.txt, a.txt, a (2).txt, sub (2), sub (2)/a.txt.
	if _, over, err := e.svc.CopyWork(ctx, e.user, docs.ID, 3); err != nil || !over {
		t.Fatalf("CopyWork over 3: %v, %v", over, err)
	}
	if b, over, err := e.svc.CopyWork(ctx, e.user, docs.ID, 6); err != nil || over || b != 20 {
		t.Fatalf("CopyWork exactly 6: %d, %v, %v", b, over, err)
	}
	if _, over, err := e.svc.CopyWork(ctx, e.user, sub.ID, 3); err != nil || over {
		t.Fatalf("CopyWork small: %v, %v", over, err)
	}
	if b, over, err := e.svc.CopyWork(ctx, e.user, f.ID, 0); err != nil || over || b != 5 {
		t.Fatalf("CopyWork file: %d, %v, %v", b, over, err)
	}

	// Into itself, or over quota: nothing is made.
	if _, err := e.svc.Copy(ctx, e.user, docs.ID, sub.ID, "", CopyOptions{}); !errors.Is(err, ErrCycle) {
		t.Fatalf("into itself: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE users SET quota_bytes = used_bytes + 3 WHERE id = $1`, e.user); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Copy(ctx, e.user, f.ID, e.root.ID, "", CopyOptions{}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over quota: %v", err)
	}
	if left, _ := e.svc.Lookup(ctx, e.user, e.root.ID, []string{"a.txt"}); left.ID != "" {
		t.Fatal("a copy over quota was kept")
	}
}
