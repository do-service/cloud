package fs

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/do-service/cloud/backend/internal/storage"
)

// stage puts content where an upload's data would be.
func (e env) stage(t *testing.T, id, content string) string {
	t.Helper()
	key := "uploads/tus/" + id
	if _, err := e.svc.store.Put(context.Background(), key, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	return key
}

func (e env) reserved(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(sum(size), 0) FROM uploads WHERE user_id = $1`, e.user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUploadLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs := e.mkdir(t, e.root.ID, "Docs")
	sum := sha256.Sum256([]byte("hello"))

	up, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: docs.ID, Name: "a.txt", Size: 5, SHA256: sum[:], OnConflict: ConflictFail})
	if err != nil || up.ID == "" || e.reserved(t) != 5 {
		t.Fatalf("reserve: %+v, %v, reserved %d", up, err, e.reserved(t))
	}
	n, err := e.svc.FinishUpload(ctx, e.user, up.ID, e.stage(t, up.ID, "hello"))
	if err != nil || n.Name != "a.txt" || n.Size != 5 || n.ParentID != docs.ID {
		t.Fatalf("finish: %+v, %v", n, err)
	}
	if got, ok := e.onDisk(t, "Docs/a.txt"); !ok || got != "hello" {
		t.Fatalf("on disk: %q, %v", got, ok)
	}
	if e.reserved(t) != 0 || e.usedBytes(t) != 5 {
		t.Fatalf("reserved %d, used %d", e.reserved(t), e.usedBytes(t))
	}
	e.checkSizes(t)
	if _, err := e.svc.GetUpload(ctx, e.user, up.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("upload row left: %v", err)
	}

	// The name is taken now: fail refuses upfront, rename picks another.
	if _, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: docs.ID, Name: "A.txt", Size: 1, OnConflict: ConflictFail}); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("reserve taken name: %v", err)
	}
	up, err = e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: docs.ID, Name: "a.txt", Size: 2, OnConflict: ConflictRename})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.FinishUpload(ctx, e.user, up.ID, e.stage(t, up.ID, "hi")); err != nil || n.Name != "a (2).txt" {
		t.Fatalf("rename on conflict: %+v, %v", n, err)
	}
}

// A name taken while the upload ran fails the finish with fail, and the
// upload keeps its reservation for the caller to cancel.
func TestUploadFinishRejects(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	reserve := func(name string, size int64, sum []byte) Upload {
		t.Helper()
		up, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: e.root.ID, Name: name, Size: size, SHA256: sum, OnConflict: ConflictFail})
		if err != nil {
			t.Fatal(err)
		}
		return up
	}
	wrong := sha256.Sum256([]byte("other"))
	for _, tc := range []struct {
		name    string
		up      Upload
		content string
		want    error
	}{
		{"checksum", reserve("c.txt", 5, wrong[:]), "hello", ErrChecksumMismatch},
		{"size", reserve("s.txt", 9, nil), "hello", ErrSizeMismatch},
	} {
		if _, err := e.svc.FinishUpload(ctx, e.user, tc.up.ID, e.stage(t, tc.up.ID, tc.content)); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	late := reserve("late.txt", 1, nil)
	e.file(t, e.root.ID, "late.txt", "x")
	if _, err := e.svc.FinishUpload(ctx, e.user, late.ID, e.stage(t, late.ID, "y")); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("taken meanwhile: %v", err)
	}
	if _, err := e.svc.GetUpload(ctx, e.user, late.ID); err != nil {
		t.Fatalf("failed finish dropped the reservation: %v", err)
	}
	if err := e.svc.CancelUpload(ctx, e.user, late.ID); err != nil || e.reserved(t) != 14 {
		t.Fatalf("cancel: %v, reserved %d", err, e.reserved(t))
	}
	e.checkSizes(t)
}

func TestUploadQuota(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.file(t, e.root.ID, "have.txt", "1234")
	if _, err := e.pool.Exec(ctx, `UPDATE users SET quota_bytes = 10 WHERE id = $1`, e.user); err != nil {
		t.Fatal(err)
	}
	reserve := func(size int64) error {
		_, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: e.root.ID, Name: "f", Size: size, OnConflict: ConflictRename})
		return err
	}
	if err := reserve(4); err != nil {
		t.Fatal(err)
	}
	// 4 used + 4 reserved: 2 bytes left.
	if err := reserve(3); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over quota: %v", err)
	}
	if err := reserve(2); err != nil {
		t.Fatalf("exactly at quota: %v", err)
	}
	// An expired reservation stops counting.
	if _, err := e.pool.Exec(ctx, `UPDATE uploads SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err := reserve(6); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

func TestUploadAccess(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	f := e.file(t, e.root.ID, "f.txt", "")
	bob := e.newUser(t, "bob")
	for what, err := range map[string]error{
		"into a file": func() error {
			_, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: f.ID, Name: "x", OnConflict: ConflictFail})
			return err
		}(),
		"into bob's view": func() error {
			_, err := e.svc.ReserveUpload(ctx, bob, Upload{ParentID: e.root.ID, Name: "x", OnConflict: ConflictFail})
			return err
		}(),
		"bad name": func() error {
			_, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: e.root.ID, Name: "a/b", OnConflict: ConflictFail})
			return err
		}(),
		"malformed parent": func() error {
			_, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: "x", Name: "x", OnConflict: ConflictFail})
			return err
		}(),
	} {
		want := map[string]error{"into a file": ErrNotDir, "bad name": ErrInvalidName}[what]
		if want == nil {
			want = ErrNotFound
		}
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", what, err, want)
		}
	}
	up, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: e.root.ID, Name: "x", OnConflict: ConflictFail})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetUpload(ctx, bob, up.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob sees alice's upload: %v", err)
	}
	if _, err := e.svc.FinishUpload(ctx, bob, up.ID, "uploads/tus/"+up.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob finishes alice's upload: %v", err)
	}
}

func TestUploadFlat(t *testing.T) {
	e := newEnv(t, func(b storage.Backend) storage.Backend { return flatStore{b} })
	ctx := context.Background()
	up, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: e.root.ID, Name: "a.txt", Size: 4, OnConflict: ConflictFail})
	if err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.FinishUpload(ctx, e.user, up.ID, e.stage(t, up.ID, "flat"))
	if err != nil {
		t.Fatal(err)
	}
	if got := e.read(t, n.ID, nil); got != "flat" {
		t.Fatalf("content %q", got)
	}
}

// FinishUploadHashed trusts the caller's sum for the content but still
// checks the size and the client's checksum.
func TestUploadFinishHashed(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	hello := sha256.Sum256([]byte("hello"))
	reserve := func(name string, size int64, sum []byte) Upload {
		t.Helper()
		up, err := e.svc.ReserveUpload(ctx, e.user, Upload{ParentID: e.root.ID, Name: name, Size: size, SHA256: sum, OnConflict: ConflictFail})
		if err != nil {
			t.Fatal(err)
		}
		return up
	}
	up := reserve("a.txt", 5, hello[:])
	n, err := e.svc.FinishUploadHashed(ctx, e.user, up.ID, e.stage(t, up.ID, "hello"), hello)
	if err != nil || n.Size != 5 || string(n.ContentHash) != string(hello[:]) {
		t.Fatalf("finish: %+v, %v", n, err)
	}
	other := sha256.Sum256([]byte("other"))
	size := reserve("s.txt", 9, nil)
	if _, err := e.svc.FinishUploadHashed(ctx, e.user, size.ID, e.stage(t, size.ID, "hello"), hello); !errors.Is(err, ErrSizeMismatch) {
		t.Errorf("size: %v", err)
	}
	sum := reserve("c.txt", 5, other[:])
	if _, err := e.svc.FinishUploadHashed(ctx, e.user, sum.ID, e.stage(t, sum.ID, "hello"), hello); !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("checksum: %v", err)
	}
	e.checkSizes(t)
}
