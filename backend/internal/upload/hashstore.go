package upload

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

// sumSuffix names the file next to an upload's data that holds the sha256
// of the data received so far, so finishing doesn't read the whole file
// again (10 GiB took 6–7 s). Its temp twin is sumSuffix+".tmp".
const sumSuffix = ".sha256"

// hashStore is tusd's filestore with sha256 computed while PATCHes write.
// The hash state lives in a sidecar file rather than memory: an upload
// resumes in a later request, possibly after a restart.
//
// A sidecar is only trusted when its offset equals the data's, which
// tusd's locker keeps stable while a chunk is written. Anything else —
// a crash between writing data and saving the state, a failed write, a
// missing or unreadable sidecar — drops it for good, and the upload falls
// back to rereading at finish: a wrong hash is worse than a slow one.
type hashStore struct {
	files filestore.FileStore
}

func newHashStore(dir string) *hashStore {
	return &hashStore{files: filestore.New(dir)}
}

// UseIn registers what tusd uses of the filestore. Concatenation and
// downloads are disabled in the handler, so they aren't offered; deferred
// length stays registered so tusd lets the request through to preCreate,
// which answers it with the API's own error.
func (s *hashStore) UseIn(composer *tusd.StoreComposer) {
	composer.UseCore(s)
	composer.UseTerminater(s)
	composer.UseLengthDeferrer(s)
}

func (s *hashStore) NewUpload(ctx context.Context, info tusd.FileInfo) (tusd.Upload, error) {
	up, err := s.files.NewUpload(ctx, info)
	if err != nil {
		return nil, err
	}
	created, err := up.GetInfo(ctx)
	if err != nil {
		return nil, err
	}
	return &hashedUpload{Upload: up, store: s, id: created.ID}, nil
}

func (s *hashStore) GetUpload(ctx context.Context, id string) (tusd.Upload, error) {
	up, err := s.files.GetUpload(ctx, id)
	if err != nil {
		return nil, err
	}
	return &hashedUpload{Upload: up, store: s, id: id}, nil
}

// The As* methods get only uploads this store made, as filestore's own do.

func (s *hashStore) AsTerminatableUpload(up tusd.Upload) tusd.TerminatableUpload {
	return up.(*hashedUpload)
}

func (s *hashStore) AsLengthDeclarableUpload(up tusd.Upload) tusd.LengthDeclarableUpload {
	return s.files.AsLengthDeclarableUpload(up.(*hashedUpload).Upload)
}

// sum returns the sha256 of upload id's data if the sidecar covers
// exactly size bytes of it; ok is false when the caller has to hash the
// data itself.
func (s *hashStore) sum(ctx context.Context, id string, size int64) (sum [32]byte, ok bool) {
	h, err := s.load(id, size)
	if err != nil {
		slog.DebugContext(ctx, "upload: no running hash, rereading", "upload", id, "err", err)
		return sum, false
	}
	h.Sum(sum[:0])
	return sum, true
}

// bootID tells sidecars written since the machine last booted from older
// ones. Within one boot the page cache shows data and sidecar consistently
// even after the process crashes, so neither is fsynced; after a power
// loss either may have lost writes the other kept, so those are rehashed.
// Where there's no boot id (not Linux) a process restart counts as a boot.
var bootID = sync.OnceValue(func() string {
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	return rand.Text()
})

// The sidecar is "{boot id}\n{offset}\n{sha256 state}".

func (s *hashStore) sidecar(id string) string {
	return filepath.Join(s.files.Path, id+sumSuffix)
}

// load returns the hash of upload id's first offset bytes from its sidecar.
func (s *hashStore) load(id string, offset int64) (hash.Hash, error) {
	raw, err := os.ReadFile(s.sidecar(id))
	if err != nil {
		return nil, err
	}
	boot, rest, ok1 := bytes.Cut(raw, []byte("\n"))
	off, state, ok2 := bytes.Cut(rest, []byte("\n"))
	if !ok1 || !ok2 {
		return nil, errors.New("malformed sidecar")
	}
	if string(boot) != bootID() {
		return nil, errors.New("sidecar from before a reboot")
	}
	if n, err := strconv.ParseInt(string(off), 10, 64); err != nil || n != offset {
		return nil, fmt.Errorf("sidecar covers %s bytes, data has %d", off, offset)
	}
	h := sha256.New()
	u, ok := h.(encoding.BinaryUnmarshaler)
	if !ok {
		return nil, errors.New("sha256 state can't be restored")
	}
	if err := u.UnmarshalBinary(state); err != nil {
		return nil, err
	}
	return h, nil
}

// save records h as the hash of upload id's first offset bytes, replacing
// the sidecar atomically so a crash leaves the old state or the new one.
func (s *hashStore) save(id string, offset int64, h hash.Hash) error {
	m, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return errors.New("sha256 state can't be saved")
	}
	state, err := m.MarshalBinary()
	if err != nil {
		return err
	}
	data := append([]byte(bootID()+"\n"+strconv.FormatInt(offset, 10)+"\n"), state...)
	tmp := s.sidecar(id) + ".tmp"
	if err := os.WriteFile(tmp, data, s.files.FileModePerm); err != nil {
		return err
	}
	return os.Rename(tmp, s.sidecar(id))
}

// drop removes upload id's sidecar and its temp file, either missing.
func (s *hashStore) drop(id string) error {
	for _, p := range []string{s.sidecar(id), s.sidecar(id) + ".tmp"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

type hashedUpload struct {
	tusd.Upload
	store *hashStore
	id    string
}

// WriteChunk hashes what the filestore writes, continuing the sidecar's
// state if it ends where this chunk starts.
func (u *hashedUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	h := sha256.New()
	if offset > 0 {
		loaded, err := u.store.load(u.id, offset)
		if err != nil {
			// The data has moved past the sidecar for good: nothing can
			// make the hash right again.
			u.drop(ctx, err)
			return u.Upload.WriteChunk(ctx, offset, src)
		}
		h = loaded
	}
	r := &hashingReader{r: src, h: h}
	n, err := u.Upload.WriteChunk(ctx, offset, r)
	if r.n != n {
		// A short write: bytes hashed that never reached the file.
		u.drop(ctx, fmt.Errorf("hashed %d bytes, wrote %d", r.n, n))
	} else if serr := u.store.save(u.id, offset+n, h); serr != nil {
		u.drop(ctx, serr)
	}
	return n, err
}

// drop gives up on the running hash. A sidecar left behind is harmless —
// data only grows, so one whose offset matches still describes it — and
// failing to remove it doesn't fail the request.
func (u *hashedUpload) drop(ctx context.Context, why error) {
	slog.DebugContext(ctx, "upload: running hash dropped", "upload", u.id, "why", why)
	if err := u.store.drop(u.id); err != nil {
		slog.DebugContext(ctx, "upload: remove sidecar", "upload", u.id, "err", err)
	}
}

// Terminate removes the sidecar along with tusd's files.
func (u *hashedUpload) Terminate(ctx context.Context) error {
	if err := u.store.files.AsTerminatableUpload(u.Upload).Terminate(ctx); err != nil {
		return err
	}
	return u.store.drop(u.id)
}

// hashingReader hashes everything read through it and counts it.
type hashingReader struct {
	r io.Reader
	h hash.Hash
	n int64
}

func (r *hashingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.h.Write(p[:n])
	r.n += int64(n)
	return n, err
}
