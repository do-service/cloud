// Package jobs runs background work on a queue in Postgres (river): jobs
// survive restarts, any number of processes can work them, and periodic
// jobs run once per interval across all of them (river elects a leader).
//
// A process's role decides its part (architecture §2): "worker" and "all"
// work jobs and schedule the periodic ones, "api" only enqueues.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
)

// Migrate brings river's own tables up to date. They are river's schema,
// versioned by river, so they stay out of our goose migrations: upgrading
// river needs no copied SQL.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Logger: slog.Default()})
	if err != nil {
		return fmt.Errorf("river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrations: %w", err)
	}
	return nil
}

// Options configure a Client.
type Options struct {
	// Work is true for the worker and all roles: jobs are worked and the
	// periodic ones scheduled. Otherwise the client only inserts.
	Work bool
	// Register adds the workers, and returns the periodic jobs among them:
	// whether one runs at all can depend on how its worker is set up.
	// Inserting a job needs its kind registered, in every role.
	Register func(*river.Workers) []*river.PeriodicJob
	// MaxWorkers bounds the jobs worked at once by this process.
	MaxWorkers int
}

// Client is river's client for this process.
type Client struct {
	*river.Client[pgx.Tx]
	work bool
}

// New makes the client; Start begins working if opts.Work.
func New(pool *pgxpool.Pool, opts Options) (*Client, error) {
	workers := river.NewWorkers()
	var periodic []*river.PeriodicJob
	if opts.Register != nil {
		periodic = opts.Register(workers)
	}
	cfg := &river.Config{
		Logger:     slog.Default().With("component", "river"),
		Workers:    workers,
		Middleware: []rivertype.Middleware{&metricsMiddleware{}},
	}
	if opts.Work {
		cfg.Queues = map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: max(opts.MaxWorkers, 1)}}
		cfg.PeriodicJobs = periodic
	}
	c, err := river.NewClient(riverpgxv5.New(pool), cfg)
	if err != nil {
		return nil, fmt.Errorf("river client: %w", err)
	}
	return &Client{Client: c, work: opts.Work}, nil
}

// Start begins working jobs; for an insert-only client it does nothing.
func (c *Client) Start(ctx context.Context) error {
	if !c.work {
		return nil
	}
	return c.Client.Start(ctx)
}

// Stop lets running jobs finish until ctx is done, then cancels them: a
// cancelled job is retried later, nothing is lost.
func (c *Client) Stop(ctx context.Context) error {
	if !c.work {
		return nil
	}
	err := c.Client.Stop(ctx)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		hard, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return c.StopAndCancel(hard)
	}
	return err
}

var (
	jobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cloud_jobs_total",
		Help: "Background jobs worked, by kind and outcome (ok, error).",
	}, []string{"kind", "outcome"})
	jobDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cloud_job_duration_seconds",
		Help:    "Background job run time, by kind.",
		Buckets: prometheus.ExponentialBuckets(0.01, 4, 10),
	}, []string{"kind"})
)

// metricsMiddleware counts and times every job, whatever its kind.
type metricsMiddleware struct{ river.MiddlewareDefaults }

func (*metricsMiddleware) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	start := time.Now()
	err := doInner(ctx)
	jobDuration.WithLabelValues(job.Kind).Observe(time.Since(start).Seconds())
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	jobsTotal.WithLabelValues(job.Kind, outcome).Inc()
	return err
}
