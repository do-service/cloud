package engine

import (
	"context"
	"time"

	"github.com/do-service/cloud/desktop/internal/api"
)

// Remote is the part of the server's API the engine uses; *api.Client is
// one, the tests' fake server another. Errors are api's (ErrPrecondition,
// ErrNotFound, ErrNameConflict, ErrCursorExpired, ErrQuota, ErrLocked,
// ErrUnauthorized, …), matched with errors.Is.
type Remote interface {
	Roots(ctx context.Context) (api.Roots, error)
	Tree(ctx context.Context, root string, fn func(api.Node) error) (string, error)
	Changes(ctx context.Context, cursor string, limit int, wait time.Duration) (api.Changes, error)
	Get(ctx context.Context, id string) (api.NodeWithPath, error)
	Mkdir(ctx context.Context, parentID, name string) (api.Node, error)
	Move(ctx context.Context, id, parentID, name, ifMatch string) (api.Node, error)
	Trash(ctx context.Context, id, ifMatch string) error
	DownloadFile(ctx context.Context, id, path string, opts api.DownloadOptions) (api.DownloadResult, error)
	Upload(ctx context.Context, u api.Upload) (api.UploadResult, error)
}

var _ Remote = (*api.Client)(nil)
