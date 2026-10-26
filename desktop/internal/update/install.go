package update

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Install is how this copy of the app can be updated.
type Install string

const (
	// InstallSelf: the updater replaces the app in place — the .app
	// bundle on macOS, cloud-desktop.exe of the per-user install (or the
	// zip) on Windows, the AppImage on Linux.
	InstallSelf Install = "self"
	// InstallManual: the app can't replace itself — a .deb/.rpm in
	// /usr/bin (the package manager owns it), a folder this user can't
	// write, a bare binary outside a bundle. Only a link to the release.
	InstallManual Install = "manual"
)

// Env is what DetectInstall looks at, from the OS in production.
type Env struct {
	GOOS       string
	Executable string
	Getenv     func(string) string
	// Writable reports whether the app may create files in dir: the
	// updater stages the download beside the app and renames it over.
	Writable func(dir string) bool
}

// OSEnv is the running process's Env.
func OSEnv(goos string) Env {
	exe, err := os.Executable()
	if err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
	}
	return Env{GOOS: goos, Executable: exe, Getenv: os.Getenv, Writable: writable}
}

// DetectInstall tells whether this copy can update itself, and what the
// updater would replace (the same target Wails' updater resolves).
func DetectInstall(env Env) (Install, string) {
	exe := filepath.Clean(env.Executable)
	if env.Executable == "" {
		return InstallManual, ""
	}
	var target string
	switch env.GOOS {
	case "darwin":
		// Only the bundle is replaced; a bare binary is a development
		// build.
		i := strings.Index(exe, ".app"+string(filepath.Separator)+"Contents"+string(filepath.Separator))
		if i < 0 {
			return InstallManual, exe
		}
		target = exe[:i+len(".app")]
	case "windows":
		target = exe
	case "linux":
		// An AppImage runs from a read-only mount ($APPDIR) of the file
		// $APPIMAGE, which is what gets replaced. Anything else came from
		// a .deb or .rpm, which the package manager updates.
		img, dir := env.Getenv("APPIMAGE"), env.Getenv("APPDIR")
		if img == "" || dir == "" || !strings.HasPrefix(exe, filepath.Clean(dir)+string(filepath.Separator)) {
			return InstallManual, exe
		}
		target = img
	default:
		return InstallManual, exe
	}
	if env.Writable == nil || !env.Writable(filepath.Dir(target)) {
		return InstallManual, target
	}
	return InstallSelf, target
}

func writable(dir string) bool {
	probe, err := os.MkdirTemp(dir, ".cloud-update-probe-*")
	if err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}

// releaseVersion is what a v* tag gives: x.y.z, maybe with a
// pre-release suffix (1.2.3-rc1).
var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// describeSuffix is what git describe adds past the last tag: commits
// since it and the hash (1.2.3-4-gabcdef0).
var describeSuffix = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+$`)

// IsRelease reports whether version is that of a released build. Others
// — 0.0.0-dev of a plain go build, 1.2.3-4-gabcdef0 of a build between
// tags — don't check for updates: by semver 1.2.3-4-g… is older than
// 1.2.3, so such a build (newer code) would be offered the release it
// started from.
func IsRelease(version string) bool {
	return releaseVersion.MatchString(version) && !describeSuffix.MatchString(version) && !strings.Contains(version, "dev")
}
