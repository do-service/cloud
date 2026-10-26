package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/do-service/cloud/desktop/internal/connection"
	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/signin"
	"github.com/do-service/cloud/desktop/internal/state"
)

func TestSetupConnection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	base, err := state.BaseDir()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := signin.Account{Server: "http://localhost:8090", UserID: "u1", Username: "ivan", DeviceID: "d1", DeviceName: "Mac"}
	conn, existing, err := setupConnection(ctx, base, a, "")
	if err != nil || existing {
		t.Fatalf("first: %+v, %v, %v", conn, existing, err)
	}
	if want := filepath.Join(home, "Cloud", "localhost-8090"); conn.LocalRoot != want {
		t.Fatalf("folder %s, want %s", conn.LocalRoot, want)
	}
	if err := localfs.CheckMarker(conn.LocalRoot, conn.ID); err != nil {
		t.Fatal(err)
	}
	path, _ := state.Path(conn.ID)
	db, err := state.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.View(ctx, func(tx *state.Tx) error {
		acc, err := tx.Account()
		if err != nil || acc.UserID != "u1" || acc.DeviceID != "d1" || acc.LocalRoot != conn.LocalRoot {
			t.Errorf("account %+v, %v", acc, err)
		}
		return nil
	})
	db.Close()

	// Signing in again keeps the connection and records the new device.
	a.DeviceID = "d2"
	again, existing, err := setupConnection(ctx, base, a, "")
	if err != nil || !existing || again.ID != conn.ID {
		t.Fatalf("again: %+v, %v, %v", again, existing, err)
	}
	if _, _, err := setupConnection(ctx, base, a, filepath.Join(home, "elsewhere")); err == nil || !strings.Contains(err.Error(), "уже подключено") {
		t.Fatalf("another folder for the same account: %v", err)
	}
	// Another account can't take a folder inside this one.
	b := signin.Account{Server: "http://localhost:8090", UserID: "u2", Username: "petr", DeviceID: "d3"}
	if _, _, err := setupConnection(ctx, base, b, filepath.Join(conn.LocalRoot, "inner")); err == nil {
		t.Fatal("nested folder accepted")
	}
	got, err := pickConnection(base, "ivan@localhost:8090")
	if err != nil || got.ID != conn.ID {
		t.Fatalf("pick: %+v, %v", got, err)
	}
	if _, err := pickConnection(base, "nobody"); err == nil {
		t.Fatal("picked a missing connection")
	}
}

// pause and resume flip the flag in state.db that a running engine
// follows (engine.Run polls it).
func TestPauseResume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	base, err := state.BaseDir()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, _, err := setupConnection(ctx, base, signin.Account{Server: "http://localhost:8090", UserID: "u1", Username: "ivan", DeviceID: "d1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	paused := func() bool {
		path, _ := state.Path(conn.ID)
		db, err := state.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var p bool
		if err := db.View(ctx, func(tx *state.Tx) error { p, err = tx.Paused(); return err }); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if paused() {
		t.Fatal("paused from the start")
	}
	if err := runPause(nil, true); err != nil || !paused() {
		t.Fatalf("pause: %v", err)
	}
	if err := runPause([]string{"ivan@localhost:8090"}, false); err != nil || paused() {
		t.Fatalf("resume: %v", err)
	}
}

// One engine per connection: a second cloudsync (or the app) on the same
// connection is refused while the first has it open.
func TestOpenSessionLocked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	base, err := state.BaseDir()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := setupConnection(ctx, base, signin.Account{Server: "http://localhost:8090", UserID: "u1", Username: "ivan", DeviceID: "d1"}, ""); err != nil {
		t.Fatal(err)
	}
	s, err := openSession(ctx, "", connection.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openSession(ctx, "", connection.Options{}); err == nil || !strings.Contains(err.Error(), "другом процессе") {
		t.Fatalf("second session: %v", err)
	}
	s.Close()
	again, err := openSession(ctx, "", connection.Options{})
	if err != nil {
		t.Fatalf("after close: %v", err)
	}
	again.Close()
}
