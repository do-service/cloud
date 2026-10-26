// Package preview makes and serves previews of images and PDFs (R6).
//
// A preview is made the first time it is asked for and kept: in storage
// at fs.PreviewKey(blob, size), with a row in previews saying whether it
// worked, so a file that can't be previewed isn't tried on every view. It
// is keyed by blob, so a new version gets a new preview and purging the
// blob drops it.
//
// The work is done by the vips command, not libvips linked in: the server
// stays a static binary without cgo, and a malformed file that crashes or
// hangs libvips only takes down a child process, which a timeout kills.
// Without vips installed there are no previews, and nothing else changes.
package preview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/storage"
)

// Size is a preview size: the longest side, in pixels.
type Size string

const (
	// Small is for lists.
	Small Size = "s"
	// Large is for viewing: what the browser shows instead of a format it
	// can't (HEIC, TIFF) or a photo of 50 megapixels.
	Large Size = "l"
)

var pixels = map[Size]int{Small: 256, Large: 2048}

func (s Size) Valid() bool { _, ok := pixels[s]; return ok }

const (
	// MaxSource is the largest file previewed: past it, reading it all
	// for a thumbnail costs more than the thumbnail is worth.
	MaxSource = 200 << 20
	// maxOutput bounds what vips may write; a 2048 px WebP is well below.
	maxOutput = 16 << 20
	timeout   = 30 * time.Second
)

// ErrNone: this file has no preview — its type, its size, a failure
// before, or no vips.
var ErrNone = errors.New("preview: none for this file")

// extensions are the types previewed, by file extension: the MIME type
// from the extension is unknown for some (HEIC) on many systems.
var extensions = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "webp": true, "gif": true, "bmp": true,
	"heic": true, "heif": true, "tif": true, "tiff": true, "pdf": true,
}

// Supported reports whether a file of this name and size gets previews.
func Supported(name string, size int64) bool {
	return size > 0 && size <= MaxSource && extensions[strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")]
}

type Service struct {
	files *fs.Service
	store storage.Backend
	db    *pgxpool.Pool
	vips  string
	// slots bounds the previews made at once: each takes a core.
	slots chan struct{}
}

// New finds vips on the PATH; without it previews are off.
func New(files *fs.Service, store storage.Backend, db *pgxpool.Pool) *Service {
	vips, err := exec.LookPath("vips")
	if err != nil {
		slog.Warn("previews off: the vips command isn't installed")
	}
	return &Service{files: files, store: store, db: db, vips: vips, slots: make(chan struct{}, runtime.NumCPU())}
}

// Enabled reports whether previews are made at all.
func (s *Service) Enabled() bool { return s != nil && s.vips != "" }

// Open returns file id's preview of size for the actor, making it first
// if needed, with the node it is of. Access is fs's: whoever may read the
// file may see its preview.
func (s *Service) Open(ctx context.Context, actorID, id string, size Size) (io.ReadCloser, fs.Node, error) {
	n, err := s.files.Get(ctx, actorID, id)
	if err != nil {
		return nil, fs.Node{}, err
	}
	if n.Kind != fs.KindFile || !s.Enabled() || !size.Valid() || !Supported(n.Name, n.Size) {
		return nil, fs.Node{}, ErrNone
	}
	ok, known, err := s.status(ctx, n.BlobID(), size)
	switch {
	case err != nil:
		return nil, fs.Node{}, err
	case known && !ok:
		return nil, fs.Node{}, ErrNone
	case !known:
		if n, err = s.make(ctx, actorID, n, size); err != nil {
			return nil, fs.Node{}, err
		}
	}
	rc, err := s.store.Get(ctx, fs.PreviewKey(n.BlobID(), string(size)), nil)
	if errors.Is(err, storage.ErrNotExist) {
		// Lost from storage: made again on the next view.
		if _, derr := s.db.Exec(ctx, `DELETE FROM previews WHERE blob_id = $1 AND size = $2`, n.BlobID(), size); derr != nil {
			return nil, fs.Node{}, derr
		}
		return nil, fs.Node{}, ErrNone
	}
	return rc, n, err
}

func (s *Service) status(ctx context.Context, blob string, size Size) (ok, known bool, err error) {
	err = s.db.QueryRow(ctx, `SELECT ok FROM previews WHERE blob_id = $1 AND size = $2`, blob, size).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	return ok, err == nil, err
}

// make makes the preview of n, or of what n's content is by now: the
// node returned is the one it is of.
func (s *Service) make(ctx context.Context, actorID string, n fs.Node, size Size) (fs.Node, error) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return fs.Node{}, ctx.Err()
	}
	rc, n, err := s.files.Open(ctx, actorID, n.ID, nil)
	if err != nil {
		return fs.Node{}, err
	}
	defer rc.Close()
	// Made meanwhile by another request.
	if ok, known, err := s.status(ctx, n.BlobID(), size); err != nil || known {
		if err == nil && !ok {
			err = ErrNone
		}
		return n, err
	}

	out, err := s.run(ctx, rc, size)
	if ctx.Err() != nil {
		// The viewer went away: nothing is known about the file.
		return fs.Node{}, ctx.Err()
	}
	if err == nil {
		_, err = s.store.Put(ctx, fs.PreviewKey(n.BlobID(), string(size)), bytes.NewReader(out), int64(len(out)))
		if err != nil {
			return fs.Node{}, fmt.Errorf("preview: store: %w", err)
		}
	} else {
		slog.InfoContext(ctx, "preview failed", "node", n.ID, "size", size, "err", err)
	}
	if _, derr := s.db.Exec(ctx, `INSERT INTO previews (blob_id, size, ok) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		n.BlobID(), size, err == nil); derr != nil {
		return fs.Node{}, fmt.Errorf("preview: record: %w", derr)
	}
	if err != nil {
		return fs.Node{}, ErrNone
	}
	return n, nil
}

// run makes a WebP of the content in, its longest side at most the
// size's: vips reads the content on stdin and writes the preview to
// stdout. It never enlarges, turns photos by their EXIF orientation, and
// renders a PDF's first page.
func (s *Service) run(ctx context.Context, in io.Reader, size Size) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.vips, "thumbnail_source", "[descriptor=0]", ".webp[Q=80,strip]",
		strconv.Itoa(pixels[size]), "--size", "down")
	cmd.Stdin = in
	var out bytes.Buffer
	var stderr limitedBuffer
	cmd.Stdout = &limitedWriter{w: &out, left: maxOutput}
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("vips: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if out.Len() == 0 {
		return nil, errors.New("vips: empty output")
	}
	return out.Bytes(), nil
}

// limitedWriter fails writes past left bytes.
type limitedWriter struct {
	w    io.Writer
	left int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > l.left {
		return 0, errors.New("preview too large")
	}
	l.left -= len(p)
	return l.w.Write(p)
}

// limitedBuffer keeps the start of what vips says on stderr, for the log.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := 1024 - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
