package update

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestDetectInstall(t *testing.T) {
	sep := string(filepath.Separator)
	p := func(parts ...string) string { return sep + filepath.Join(parts...) }
	env := func(goos, exe string, vars map[string]string, writable bool) Env {
		return Env{GOOS: goos, Executable: exe, Getenv: func(k string) string { return vars[k] }, Writable: func(string) bool { return writable }}
	}
	appImage := map[string]string{"APPIMAGE": p("home", "u", "Apps", "Cloud.AppImage"), "APPDIR": p("tmp", ".mount_Cloud1")}
	for _, tc := range []struct {
		name   string
		env    Env
		want   Install
		target string
	}{
		{"mac app", env("darwin", p("Applications", "Облако.app", "Contents", "MacOS", "cloud-desktop"), nil, true), InstallSelf, p("Applications", "Облако.app")},
		{"mac app, folder not writable", env("darwin", p("Applications", "Облако.app", "Contents", "MacOS", "cloud-desktop"), nil, false), InstallManual, p("Applications", "Облако.app")},
		{"mac bare binary", env("darwin", p("src", "desktop", "bin", "cloud-desktop"), nil, true), InstallManual, p("src", "desktop", "bin", "cloud-desktop")},
		{"windows per-user install", env("windows", p("Users", "u", "AppData", "Local", "Programs", "Cloud", "cloud-desktop.exe"), nil, true), InstallSelf, p("Users", "u", "AppData", "Local", "Programs", "Cloud", "cloud-desktop.exe")},
		{"windows, folder not writable", env("windows", p("Program Files", "Cloud", "cloud-desktop.exe"), nil, false), InstallManual, p("Program Files", "Cloud", "cloud-desktop.exe")},
		{"appimage", env("linux", p("tmp", ".mount_Cloud1", "usr", "bin", "cloud-desktop"), appImage, true), InstallSelf, p("home", "u", "Apps", "Cloud.AppImage")},
		{"appimage, folder not writable", env("linux", p("tmp", ".mount_Cloud1", "usr", "bin", "cloud-desktop"), appImage, false), InstallManual, p("home", "u", "Apps", "Cloud.AppImage")},
		{"deb or rpm", env("linux", p("usr", "bin", "cloud-desktop"), nil, true), InstallManual, p("usr", "bin", "cloud-desktop")},
		// APPIMAGE is inherited by what an AppImage starts.
		{"deb with an inherited APPIMAGE", env("linux", p("usr", "bin", "cloud-desktop"), appImage, true), InstallManual, p("usr", "bin", "cloud-desktop")},
		{"freebsd", env("freebsd", p("usr", "local", "bin", "cloud-desktop"), nil, true), InstallManual, p("usr", "local", "bin", "cloud-desktop")},
	} {
		got, target := DetectInstall(tc.env)
		if got != tc.want || target != tc.target {
			t.Errorf("%s: %s %s, want %s %s", tc.name, got, target, tc.want, tc.target)
		}
	}
	if got, _ := DetectInstall(Env{GOOS: "darwin"}); got != InstallManual {
		t.Errorf("no executable: %s", got)
	}
}

func TestExtractedAppImage(t *testing.T) {
	sep := string(filepath.Separator)
	p := func(parts ...string) string { return sep + filepath.Join(parts...) }
	img := p("home", "u", "Apps", "Cloud.AppImage")
	extracted := p("tmp", "appimage_extracted_0123abcd")
	env := func(goos, exe, appDir string) Env {
		vars := map[string]string{"APPIMAGE": img, "APPDIR": appDir}
		return Env{GOOS: goos, Executable: exe, Getenv: func(k string) string { return vars[k] }}
	}
	for _, tc := range []struct {
		name string
		env  Env
		want bool
	}{
		{"extracted", env("linux", filepath.Join(extracted, "usr", "bin", "cloud-desktop"), extracted), true},
		{"mounted", env("linux", p("tmp", ".mount_Cloud1", "usr", "bin", "cloud-desktop"), p("tmp", ".mount_Cloud1")), false},
		// APPDIR is inherited by what an AppImage starts.
		{"deb with an inherited APPDIR", env("linux", p("usr", "bin", "cloud-desktop"), extracted), false},
		{"not linux", env("darwin", filepath.Join(extracted, "usr", "bin", "cloud-desktop"), extracted), false},
	} {
		if got := ExtractedAppImage(tc.env); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestExtractAndRun(t *testing.T) {
	t.Setenv("APPIMAGE_EXTRACT_AND_RUN", "")
	eng := &envEngine{}
	if err := ExtractAndRun(eng).Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen := eng.seen; seen != "1" {
		t.Fatalf("the helper would start with APPIMAGE_EXTRACT_AND_RUN=%q", seen)
	}
}

// envEngine records the environment Wails' Restart would hand the helper.
type envEngine struct {
	fakeEngine
	seen string
}

func (e *envEngine) Restart(context.Context) error {
	e.seen = os.Getenv("APPIMAGE_EXTRACT_AND_RUN")
	return nil
}

func TestWritable(t *testing.T) {
	dir := t.TempDir()
	if !writable(dir) {
		t.Fatal("a temp dir isn't writable")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the probe stayed: %v", entries)
	}
	if writable(filepath.Join(dir, "missing")) {
		t.Fatal("a missing dir is writable")
	}
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		ro := filepath.Join(dir, "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		if writable(ro) {
			t.Fatal("a read-only dir is writable")
		}
	}
}

func TestIsRelease(t *testing.T) {
	for v, want := range map[string]bool{
		"1.2.3":             true,
		"0.0.1":             true,
		"10.20.30":          true,
		"1.2.3-rc1":         true,
		"1.2.3-beta.2":      true,
		"0.0.0-dev":         false,
		"1.2.3-4-gabcdef0":  false,
		"1.2.3-rc1-2-g1234": false,
		"1.2":               false,
		"v1.2.3":            false,
		"":                  false,
	} {
		if got := IsRelease(v); got != want {
			t.Errorf("IsRelease(%q) = %v", v, got)
		}
	}
}

func TestSweepStaging(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Облако.app")
	now := time.Now()
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Join(p, "x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := mk("wails-update-1", 2*time.Hour)
	fresh := mk("wails-update-2", time.Minute)
	other := mk("Other.app", 2*time.Hour)
	mk("Облако.app", 2*time.Hour)
	SweepStaging(target, now)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("an old staging dir stayed: %v", err)
	}
	for _, p := range []string{fresh, other, target} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s went: %v", p, err)
		}
	}
}
