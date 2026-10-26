// Package maintenance holds the periodic housekeeping jobs over the file
// tree: emptying the trash past its retention (E4-4), trimming file
// history to the version policy (E4-2) and the sync journal to its
// retention (E11-6). fs decides what goes; this package
// only schedules it.
package maintenance

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/fs"
)

// Interval is how often each job runs.
const Interval = time.Hour

// batch bounds the work of one step, so one transaction or one loop
// iteration never grows with the backlog. A variable for tests.
var batch = 500

// TrashArgs is the periodic trash retention job.
type TrashArgs struct{}

func (TrashArgs) Kind() string { return "trash_retention" }

// TrashWorker purges trash items older than Retention.
type TrashWorker struct {
	river.WorkerDefaults[TrashArgs]
	FS        *fs.Service
	Retention time.Duration
}

func (w *TrashWorker) Work(ctx context.Context, _ *river.Job[TrashArgs]) error {
	cutoff := time.Now().Add(-w.Retention)
	purged := 0
	defer func() {
		if purged > 0 {
			slog.InfoContext(ctx, "trash retention", "purged", purged)
		}
	}()
	for {
		refs, err := w.FS.ExpiredTrash(ctx, cutoff, batch)
		if err != nil {
			return err
		}
		for _, r := range refs {
			// Gone already: purged by its owner meanwhile, or along with
			// a folder it was in.
			if err := w.FS.Purge(ctx, r.OwnerID, r.ID); err != nil && !errors.Is(err, fs.ErrNotFound) {
				return err
			}
			purged++
		}
		if len(refs) < batch {
			return nil
		}
	}
}

// VersionsArgs is the periodic version policy job.
type VersionsArgs struct{}

func (VersionsArgs) Kind() string { return "version_prune" }

// VersionsWorker deletes the versions Policy no longer keeps.
type VersionsWorker struct {
	river.WorkerDefaults[VersionsArgs]
	FS     *fs.Service
	Policy fs.VersionPolicy
}

func (w *VersionsWorker) Work(ctx context.Context, _ *river.Job[VersionsArgs]) error {
	total := 0
	for {
		n, err := w.FS.PruneVersions(ctx, w.Policy, batch)
		total += n
		if err != nil || n < batch {
			if total > 0 {
				slog.InfoContext(ctx, "version policy", "pruned", total)
			}
			return err
		}
	}
}

// Setup registers the workers and returns the periodic jobs for the
// policies in effect: a disabled policy schedules nothing.
func Setup(workers *river.Workers, files *fs.Service, trashRetention time.Duration, versions fs.VersionPolicy) []*river.PeriodicJob {
	var periodic []*river.PeriodicJob
	if trashRetention > 0 {
		river.AddWorker(workers, &TrashWorker{FS: files, Retention: trashRetention})
		periodic = append(periodic, every(TrashArgs{}))
	}
	if versions.MaxCount > 0 || versions.MaxAge > 0 {
		river.AddWorker(workers, &VersionsWorker{FS: files, Policy: versions})
		periodic = append(periodic, every(VersionsArgs{}))
	}
	return periodic
}

// every schedules args each Interval and once at start, so a restart after
// a long outage catches up right away.
func every(args river.JobArgs) *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(Interval), func() (river.JobArgs, *river.InsertOpts) {
		return args, nil
	}, &river.PeriodicJobOpts{RunOnStart: true})
}

// AuditArgs is the periodic audit log retention job.
type AuditArgs struct{}

func (AuditArgs) Kind() string { return "audit_retention" }

// AuditWorker deletes audit log entries older than Retention.
type AuditWorker struct {
	river.WorkerDefaults[AuditArgs]
	Log       *audit.Log
	Retention time.Duration
}

func (w *AuditWorker) Work(ctx context.Context, _ *river.Job[AuditArgs]) error {
	cutoff := time.Now().Add(-w.Retention)
	var total int64
	for {
		n, err := w.Log.Prune(ctx, cutoff, batch)
		if err != nil {
			return err
		}
		total += n
		if n < int64(batch) {
			if total > 0 {
				slog.InfoContext(ctx, "audit retention", "deleted", total)
			}
			return nil
		}
	}
}

// JournalArgs is the periodic sync journal retention job.
type JournalArgs struct{}

func (JournalArgs) Kind() string { return "sync_journal_retention" }

// JournalWorker deletes sync journal rows older than Retention; clients
// with a cursor from before then resync in full (E11-6).
type JournalWorker struct {
	river.WorkerDefaults[JournalArgs]
	FS        *fs.Service
	Retention time.Duration
}

func (w *JournalWorker) Work(ctx context.Context, _ *river.Job[JournalArgs]) error {
	cutoff := time.Now().Add(-w.Retention)
	total := 0
	for {
		n, err := w.FS.PruneJournal(ctx, cutoff, batch)
		total += n
		if err != nil || n < batch {
			if total > 0 {
				slog.InfoContext(ctx, "sync journal retention", "deleted", total)
			}
			return err
		}
	}
}

// SetupJournal registers sync journal retention; with retention 0 the
// journal is kept for good and nothing is scheduled.
func SetupJournal(workers *river.Workers, files *fs.Service, retention time.Duration) []*river.PeriodicJob {
	if retention <= 0 {
		return nil
	}
	river.AddWorker(workers, &JournalWorker{FS: files, Retention: retention})
	return []*river.PeriodicJob{every(JournalArgs{})}
}

// SetupAudit registers audit retention; with retention 0 entries are kept
// for good and nothing is scheduled.
func SetupAudit(workers *river.Workers, log *audit.Log, retention time.Duration) []*river.PeriodicJob {
	if retention <= 0 {
		return nil
	}
	river.AddWorker(workers, &AuditWorker{Log: log, Retention: retention})
	return []*river.PeriodicJob{every(AuditArgs{})}
}
