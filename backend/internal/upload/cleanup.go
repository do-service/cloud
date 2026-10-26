package upload

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/riverqueue/river"

	cloudfs "github.com/do-service/cloud/backend/internal/fs"
)

// OrphanAge is how long a file in uploads/ may sit untouched without an
// upload behind it before the cleanup takes it for a leftover. Files
// being written are touched all the time, so this only catches the dead.
const OrphanAge = 24 * time.Hour

// CleanupInterval is how often the cleanup runs.
const CleanupInterval = time.Hour

// CleanupArgs is the periodic job that removes abandoned uploads (E3-5).
type CleanupArgs struct{}

func (CleanupArgs) Kind() string { return "upload_cleanup" }

// CleanupWorker runs Cleanup as a job.
type CleanupWorker struct {
	river.WorkerDefaults[CleanupArgs]
	FS      *cloudfs.Service
	DataDir string
}

func (w *CleanupWorker) Work(ctx context.Context, _ *river.Job[CleanupArgs]) error {
	n, err := Cleanup(ctx, w.FS, w.DataDir, OrphanAge)
	if n > 0 {
		slog.InfoContext(ctx, "upload cleanup", "removed", n)
	}
	return err
}

// PeriodicCleanup schedules the cleanup every CleanupInterval, and once
// right at start: a restart after a long outage shouldn't wait an hour.
func PeriodicCleanup() *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(CleanupInterval), func() (river.JobArgs, *river.InsertOpts) {
		return CleanupArgs{}, nil
	}, &river.PeriodicJobOpts{RunOnStart: true})
}

// Cleanup removes what abandoned uploads leave behind and returns how many
// it removed:
//   - uploads whose reservation expired, with their tus data;
//   - tus data and hash sidecars no upload refers to (a crash between the
//     steps of creating or finishing one);
//   - staging files directly in uploads/ (fs.CreateFile's, storage's
//     temp files) that nothing has written to for orphanAge.
func Cleanup(ctx context.Context, files *cloudfs.Service, dataDir string, orphanAge time.Duration) (int, error) {
	tusDir := filepath.Join(dataDir, filepath.FromSlash(stagingArea))
	removed := 0
	for {
		ids, err := files.ExpiredUploads(ctx, 100)
		if err != nil {
			return removed, err
		}
		for _, id := range ids {
			if err := removeTusData(tusDir, id); err != nil {
				return removed, err
			}
			if err := files.DropUpload(ctx, id); err != nil {
				return removed, err
			}
			removed++
		}
		if len(ids) < 100 {
			break
		}
	}

	cutoff := time.Now().Add(-orphanAge)
	entries, err := os.ReadDir(tusDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return removed, err
	}
	seen := map[string]bool{}
	for _, e := range entries {
		id := uploadIDOf(e.Name())
		if seen[id] || !old(e, cutoff) {
			continue
		}
		seen[id] = true
		exists, err := files.UploadExists(ctx, id)
		if err != nil {
			return removed, err
		}
		if exists {
			continue
		}
		if err := removeTusData(tusDir, id); err != nil {
			return removed, err
		}
		removed++
	}

	staging, err := os.ReadDir(filepath.Join(dataDir, "uploads"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return removed, err
	}
	for _, e := range staging {
		if !e.Type().IsRegular() || !old(e, cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dataDir, "uploads", e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// old reports whether e was last written before cutoff; one that vanished
// meanwhile counts as not old, leaving it alone.
func old(e os.DirEntry, cutoff time.Time) bool {
	info, err := e.Info()
	return err == nil && info.ModTime().Before(cutoff)
}

// uploadIDOf is the upload a file in the tus directory belongs to: its
// data, .info, or hash sidecar (hashStore).
func uploadIDOf(name string) string {
	for _, suffix := range []string{".info", sumSuffix + ".tmp", sumSuffix} {
		if id, ok := strings.CutSuffix(name, suffix); ok {
			return id
		}
	}
	return name
}

// removeTusData deletes an upload's data, .info file and hash sidecar,
// any of them missing.
func removeTusData(dir, id string) error {
	if strings.ContainsAny(id, `/\`) || id == "" || id == "." || id == ".." {
		return fmt.Errorf("upload cleanup: odd upload id %q", id)
	}
	for _, name := range []string{id, id + ".info", id + sumSuffix, id + sumSuffix + ".tmp"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
