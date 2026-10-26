package jobs

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/riverqueue/river"

	"github.com/do-service/cloud/backend/internal/db"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://cloud:cloud@localhost:55433/cloud_test?sslmode=disable"
	}
	if err := db.Migrate(url); err != nil {
		t.Skipf("skipping: test database unavailable (%v)", err)
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, url, 8, 0, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM river_job`); err != nil {
		t.Fatal(err)
	}
	return pool
}

type pingArgs struct{}

func (pingArgs) Kind() string { return "test_ping" }

type pingWorker struct {
	river.WorkerDefaults[pingArgs]
	runs *atomic.Int32
}

func (w *pingWorker) Work(context.Context, *river.Job[pingArgs]) error {
	w.runs.Add(1)
	return nil
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// An api process enqueues without working; a worker process picks the job
// up, and works the periodic ones as well.
func TestRoles(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	var runs atomic.Int32
	register := func(w *river.Workers) []*river.PeriodicJob {
		river.AddWorker(w, &pingWorker{runs: &runs})
		return nil
	}

	api, err := New(pool, Options{Work: false, Register: register})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Insert(ctx, pingArgs{}, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if runs.Load() != 0 {
		t.Fatal("an api client worked a job")
	}

	before := testutil.ToFloat64(jobsTotal.WithLabelValues("test_ping", "ok"))
	periodic := river.NewPeriodicJob(river.PeriodicInterval(time.Hour), func() (river.JobArgs, *river.InsertOpts) {
		return pingArgs{}, nil
	}, &river.PeriodicJobOpts{RunOnStart: true})
	worker, err := New(pool, Options{Work: true, MaxWorkers: 2, Register: func(w *river.Workers) []*river.PeriodicJob {
		register(w)
		return []*river.PeriodicJob{periodic}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = worker.Stop(stop)
	})
	// The enqueued job and the periodic one's first run.
	eventually(t, "both jobs worked", func() bool { return runs.Load() == 2 })
	eventually(t, "metrics", func() bool { return testutil.ToFloat64(jobsTotal.WithLabelValues("test_ping", "ok"))-before == 2 })
}
