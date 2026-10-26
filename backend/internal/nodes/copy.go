package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/httpx"
)

// Copies (E1-6) small enough are made within the request; bigger ones go
// to a background job, whose state the UI asks about. Variables for tests.
var (
	copyNowNodes       = 1000
	copyNowBytes int64 = 1 << 30
)

// CopyArgs is a copy made in the background.
type CopyArgs struct {
	ActorID  string `json:"actorId"`
	SourceID string `json:"sourceId"`
	DestID   string `json:"destId"`
	Name     string `json:"name"`
}

func (CopyArgs) Kind() string { return "copy" }

// CopyWorker makes background copies.
type CopyWorker struct {
	river.WorkerDefaults[CopyArgs]
	FS *fs.Service
}

func (w *CopyWorker) Work(ctx context.Context, job *river.Job[CopyArgs]) error {
	a := job.Args
	_, err := w.FS.Copy(ctx, a.ActorID, a.SourceID, a.DestID, a.Name, fs.CopyOptions{Rename: true, Unbounded: true})
	if err == nil {
		return nil
	}
	// What retrying can't fix (rights, quota, a folder gone) ends the job
	// with its error code, which the UI shows; anything else is retried,
	// the half-made copy having been removed.
	if body, status := apiError(err); status < http.StatusInternalServerError {
		return river.JobCancel(errors.New(body.Code))
	}
	return err
}

// Timeout: a folder of many gigabytes takes a while.
func (w *CopyWorker) Timeout(*river.Job[CopyArgs]) time.Duration { return 12 * time.Hour }

// CopyJobs runs background copies; RiverCopyJobs in production.
type CopyJobs interface {
	Enqueue(ctx context.Context, a CopyArgs) (int64, error)
	// Status is the job's state: "running", "done" or "failed" with an
	// error code; ErrNoJob unless the actor started it.
	Status(ctx context.Context, actorID string, id int64) (state, code string, err error)
}

var ErrNoJob = errors.New("nodes: no such job")

// RiverCopyJobs is CopyJobs on the river queue.
type RiverCopyJobs struct {
	Client *river.Client[pgx.Tx]
}

func (j RiverCopyJobs) Enqueue(ctx context.Context, a CopyArgs) (int64, error) {
	res, err := j.Client.Insert(ctx, a, nil)
	if err != nil {
		return 0, err
	}
	return res.Job.ID, nil
}

// errorCode is an API error code, as CopyWorker cancels a job with.
var apiCode = regexp.MustCompile(`^[a-z_]+$`)

func (j RiverCopyJobs) Status(ctx context.Context, actorID string, id int64) (string, string, error) {
	job, err := j.Client.JobGet(ctx, id)
	if errors.Is(err, rivertype.ErrNotFound) {
		return "", "", ErrNoJob
	}
	if err != nil {
		return "", "", err
	}
	var a CopyArgs
	if job.Kind != (CopyArgs{}).Kind() || json.Unmarshal(job.EncodedArgs, &a) != nil || a.ActorID != actorID {
		return "", "", ErrNoJob
	}
	switch job.State {
	case rivertype.JobStateCompleted:
		return "done", "", nil
	case rivertype.JobStateCancelled, rivertype.JobStateDiscarded:
		code := httpx.CodeInternal
		if n := len(job.Errors); n > 0 {
			if c := strings.TrimPrefix(job.Errors[n-1].Error, "JobCancelError: "); apiCode.MatchString(c) {
				code = c
			}
		}
		return "failed", code, nil
	}
	return "running", "", nil
}

// copyNode copies one node for the batch API: here and now when small,
// else as a job, whose id comes back instead of the node.
func (h *Handlers) copyNode(r *http.Request, u auth.User, op batchOp) (fs.Node, int64, error) {
	ctx := r.Context()
	parent, err := h.resolve(ctx, u, op.ParentID)
	if err != nil {
		return fs.Node{}, 0, err
	}
	if h.Jobs != nil {
		bytes, many, err := h.FS.CopyWork(ctx, u.ID, op.ID, copyNowNodes)
		if err != nil {
			return fs.Node{}, 0, err
		}
		if many || bytes >= copyNowBytes {
			id, err := h.Jobs.Enqueue(ctx, CopyArgs{ActorID: u.ID, SourceID: op.ID, DestID: parent, Name: op.Name})
			return fs.Node{}, id, err
		}
	}
	n, err := h.FS.Copy(ctx, u.ID, op.ID, parent, op.Name, fs.CopyOptions{Rename: true})
	return n, 0, err
}

// jobStatus tells the UI how a background copy goes.
func (h *Handlers) jobStatus(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such job")
		return
	}
	state, code, err := h.Jobs.Status(r.Context(), u.ID, id)
	if errors.Is(err, ErrNoJob) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such job")
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := map[string]any{"id": id, "state": state}
	if code != "" {
		resp["error"] = code
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}
