package nodes

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/riverqueue/river"

	"github.com/do-service/cloud/backend/internal/jobs"
)

type batchReply struct {
	Results []struct {
		ID    string    `json:"id"`
		Node  *nodeJSON `json:"node"`
		Job   *int64    `json:"job"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	} `json:"results"`
}

func (e env) copyOp(t *testing.T, id, parent string) batchReply {
	t.Helper()
	rec := e.send(t, "POST", "/api/v1/nodes/batch", `{"ops":[{"op":"copy","id":"`+id+`","parentId":"`+parent+`"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch copy: %d %s", rec.Code, rec.Body)
	}
	var out batchReply
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCopyNow(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	docs, _ := e.svc.CreateDir(ctx, e.user, e.root.ID, "docs")
	f, _ := e.svc.CreateFile(ctx, e.user, docs.ID, "a.txt", strings.NewReader("hi"), 2)
	// Into its own folder: beside it, under a free name.
	out := e.copyOp(t, f.ID, docs.ID)
	if r := out.Results[0]; r.Node == nil || r.Node.Name != "a (2).txt" || r.Job != nil {
		t.Fatalf("copy: %+v", out)
	}
	out = e.copyOp(t, docs.ID, "root")
	if r := out.Results[0]; r.Node == nil || r.Node.Name != "docs (2)" {
		t.Fatalf("folder copy: %+v", out)
	}
	if rec := e.send(t, "POST", "/api/v1/nodes/batch", `{"ops":[{"op":"copy","id":"`+f.ID+`"}]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("copy without a destination: %d", rec.Code)
	}
	out = e.copyOp(t, docs.ID, docs.ID)
	if r := out.Results[0]; r.Error == nil || r.Error.Code != "move_into_itself" {
		t.Fatalf("into itself: %+v", out.Results[0])
	}
}

// Big copies go to a job, whose state the UI follows.
func TestCopyInBackground(t *testing.T) {
	pool := newEnv(t, nil).pool
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM river_job`); err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(pool, jobs.Options{Register: func(w *river.Workers) []*river.PeriodicJob {
		river.AddWorker(w, &CopyWorker{})
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	testJobs = RiverCopyJobs{Client: queue.Client}
	saved := copyNowNodes
	copyNowNodes = 1
	t.Cleanup(func() { testJobs, copyNowNodes = nil, saved })

	e := newEnv(t, nil)
	docs, _ := e.svc.CreateDir(ctx, e.user, e.root.ID, "docs")
	for _, n := range []string{"a.txt", "b.txt"} {
		if _, err := e.svc.CreateFile(ctx, e.user, docs.ID, n, strings.NewReader(n), int64(len(n))); err != nil {
			t.Fatal(err)
		}
	}
	out := e.copyOp(t, docs.ID, "root")
	r := out.Results[0]
	if r.Job == nil || r.Node != nil {
		t.Fatalf("big copy not in the background: %+v", out)
	}
	job := strconv.FormatInt(*r.Job, 10)
	status := func() map[string]any {
		rec := e.send(t, "GET", "/api/v1/jobs/"+job, "")
		var s map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &s)
		return s
	}
	if s := status(); s["state"] != "running" {
		t.Fatalf("queued: %v", s)
	}

	// The worker makes it, renamed beside the original.
	var args CopyArgs
	if err := pool.QueryRow(ctx, `SELECT args FROM river_job WHERE id = $1`, *r.Job).Scan(&args); err != nil {
		t.Fatal(err)
	}
	w := &CopyWorker{FS: e.svc}
	if err := w.Work(ctx, &river.Job[CopyArgs]{Args: args}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.Lookup(ctx, e.user, e.root.ID, []string{"docs (2)", "b.txt"}); err != nil || n.Size != 5 {
		t.Fatalf("copied: %+v, %v", n, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1`, *r.Job); err != nil {
		t.Fatal(err)
	}
	if s := status(); s["state"] != "done" {
		t.Fatalf("done: %v", s)
	}

	// What retrying won't fix ends the job with the error's code.
	if _, err := e.pool.Exec(ctx, `UPDATE users SET quota_bytes = used_bytes WHERE id = $1`, e.user); err != nil {
		t.Fatal(err)
	}
	err = w.Work(ctx, &river.Job[CopyArgs]{Args: args})
	if err == nil || err.Error() != "JobCancelError: quota_exceeded" {
		t.Fatalf("over quota: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE river_job SET state = 'cancelled', finalized_at = now(),
		errors = ARRAY[jsonb_build_object('at', now(), 'attempt', 1, 'error', $2::text, 'trace', '')] WHERE id = $1`, *r.Job, err.Error()); err != nil {
		t.Fatal(err)
	}
	if s := status(); s["state"] != "failed" || s["error"] != "quota_exceeded" {
		t.Fatalf("failed: %v", s)
	}

	// Someone else's job is no one's business.
	if _, err := pool.Exec(ctx, `UPDATE river_job SET args = jsonb_set(args, '{actorId}', '"00000000-0000-4000-8000-000000000000"') WHERE id = $1`, *r.Job); err != nil {
		t.Fatal(err)
	}
	if rec := e.send(t, "GET", "/api/v1/jobs/"+job, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("someone else's job: %d", rec.Code)
	}
}
