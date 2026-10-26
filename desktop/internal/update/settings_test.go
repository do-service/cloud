package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestModePersistence(t *testing.T) {
	base := filepath.Join(t.TempDir(), "Cloud")
	if m, err := LoadMode(base); err != nil || m != ModeAuto {
		t.Fatalf("no file: %s %v", m, err)
	}
	if err := SaveMode(base, ModeOff); err != nil {
		t.Fatal(err)
	}
	if m, _ := LoadMode(base); m != ModeOff {
		t.Fatalf("after saving off: %s", m)
	}

	// Other settings in the file stay as they are.
	path := filepath.Join(base, SettingsFile)
	if err := os.WriteFile(path, []byte(`{"autostart":true,"updates":"off"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveMode(base, ModeNotify); err != nil {
		t.Fatal(err)
	}
	var all map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &all); err != nil || all["autostart"] != true || all["updates"] != "notify" {
		t.Fatalf("file after saving: %s", data)
	}
	if left, _ := filepath.Glob(filepath.Join(base, "*.tmp")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}

	// A newer client's mode reads as the default; a bad one isn't saved.
	if err := os.WriteFile(path, []byte(`{"updates":"nightly"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if m, err := LoadMode(base); err != nil || m != DefaultMode {
		t.Fatalf("unknown mode: %s %v", m, err)
	}
	if err := SaveMode(base, "nightly"); err == nil {
		t.Fatal("saved an unknown mode")
	}
	// A broken file: the default, and saving replaces it.
	if err := os.WriteFile(path, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if m, err := LoadMode(base); err == nil || m != DefaultMode {
		t.Fatalf("broken file: %s %v", m, err)
	}
	if err := SaveMode(base, ModeAuto); err != nil {
		t.Fatal(err)
	}
	if m, err := LoadMode(base); err != nil || m != ModeAuto {
		t.Fatalf("after replacing a broken file: %s %v", m, err)
	}
}
