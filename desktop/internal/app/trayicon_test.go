package app

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestTrayIcons(t *testing.T) {
	seen := map[string]Status{}
	for _, s := range []Status{StatusSynced, StatusSyncing, StatusPaused, StatusError, StatusOffline} {
		b := TrayIcon(s)
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil || img.Bounds().Dx() != iconSize {
			t.Fatalf("%s: %v", s, err)
		}
		if o, dup := seen[string(b)]; dup {
			t.Fatalf("%s looks like %s", s, o)
		}
		seen[string(b)] = s
		// TRAY_ICON_DIR=… go test -run TrayIcons: the icons to look at.
		if dir := os.Getenv("TRAY_ICON_DIR"); dir != "" {
			_ = os.WriteFile(filepath.Join(dir, string(s)+".png"), b, 0o644)
		}
	}
	if !bytes.Equal(TrayIcon("unknown"), TrayIcon(StatusSynced)) {
		t.Fatal("an unknown state has no icon")
	}
}
