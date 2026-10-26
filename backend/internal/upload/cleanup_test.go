package upload

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tusDir := filepath.Join(e.dataDir, "uploads", "tus")
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(e.dataDir, filepath.FromSlash(rel)))
		return err == nil
	}
	age := func(rel string, d time.Duration) {
		t.Helper()
		then := time.Now().Add(-d)
		if err := os.Chtimes(filepath.Join(e.dataDir, filepath.FromSlash(rel)), then, then); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(e.dataDir, filepath.FromSlash(rel)), []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	_, expired := e.create(t, 10, 201, "filename", "expired.bin")
	_, live := e.create(t, 10, 201, "filename", "live.bin")
	expiredID, liveID := filepath.Base(expired), filepath.Base(live)
	// Both get a hash sidecar.
	e.patch(t, "alice", expired, 0, "01234")
	e.patch(t, "alice", live, 0, "01234")
	if _, err := e.pool.Exec(ctx, `UPDATE uploads SET expires_at = now() - interval '1 minute' WHERE id = $1`, expiredID); err != nil {
		t.Fatal(err)
	}
	// An upload untouched for days but still reserved is left alone.
	age("uploads/tus/"+liveID, 48*time.Hour)
	age("uploads/tus/"+liveID+".sha256", 48*time.Hour)
	// Data no upload refers to: old goes, recent stays (it may be one
	// being created right now).
	write("uploads/tus/00000000-0000-0000-0000-00000000dead")
	write("uploads/tus/00000000-0000-0000-0000-00000000dead.info")
	write("uploads/tus/00000000-0000-0000-0000-00000000dead.sha256")
	write("uploads/tus/00000000-0000-0000-0000-00000000dead.sha256.tmp")
	age("uploads/tus/00000000-0000-0000-0000-00000000dead", 48*time.Hour)
	age("uploads/tus/00000000-0000-0000-0000-00000000dead.info", 48*time.Hour)
	age("uploads/tus/00000000-0000-0000-0000-00000000dead.sha256", 48*time.Hour)
	age("uploads/tus/00000000-0000-0000-0000-00000000dead.sha256.tmp", 48*time.Hour)
	write("uploads/tus/00000000-0000-0000-0000-0000000fresh")
	// Staging leftovers of a crashed CreateFile or Put.
	write("uploads/fs-stale")
	age("uploads/fs-stale", 48*time.Hour)
	write("uploads/.put-busy")

	n, err := Cleanup(ctx, e.files, e.dataDir, OrphanAge)
	if err != nil || n != 3 {
		t.Fatalf("cleanup: removed %d, %v", n, err)
	}
	for rel, want := range map[string]bool{
		"uploads/tus/" + expiredID:                                    false,
		"uploads/tus/" + expiredID + ".info":                          false,
		"uploads/tus/" + expiredID + ".sha256":                        false,
		"uploads/tus/" + liveID + ".sha256":                           true,
		"uploads/tus/00000000-0000-0000-0000-00000000dead.sha256":     false,
		"uploads/tus/00000000-0000-0000-0000-00000000dead.sha256.tmp": false,
		"uploads/tus/" + liveID:                                       true,
		"uploads/tus/" + liveID + ".info":                             true,
		"uploads/tus/00000000-0000-0000-0000-00000000dead":            false,
		"uploads/tus/00000000-0000-0000-0000-00000000dead.info":       false,
		"uploads/tus/00000000-0000-0000-0000-0000000fresh":            true,
		"uploads/fs-stale":                                            false,
		"uploads/.put-busy":                                           true,
		"uploads/tus":                                                 true,
	} {
		if exists(rel) != want {
			t.Errorf("%s exists = %v, want %v", rel, !want, want)
		}
	}
	if e.reserved(t) != 1 {
		t.Errorf("reservations left: %d, want the live one", e.reserved(t))
	}
	if _, err := os.Stat(tusDir); err != nil {
		t.Fatal(err)
	}
}
