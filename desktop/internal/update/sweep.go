package update

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staleAfter: a staging directory this old has no helper waiting on it.
// A fresh one may be the helper's that just started this process (it
// removes its own after the swap).
const staleAfter = time.Hour

// SweepStaging removes what downloads left beside target (the app the
// updater replaces): the updater stages each in a wails-update-* directory
// there and removes it only when installed or downloaded again, so one
// the user never installed would stay, in /Applications say, for good.
func SweepStaging(target string, now time.Time) {
	if target == "" {
		return
	}
	dir := filepath.Dir(target)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "wails-update-") {
			continue
		}
		fi, err := e.Info()
		if err != nil || now.Sub(fi.ModTime()) < staleAfter {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}
