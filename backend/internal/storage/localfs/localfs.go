// Package localfs stores content as plain files under the data directory,
// laid out so an admin can read it without the database (architecture §4):
//
//	files/{user_id}/...    current content, mirroring each user's tree
//	versions/{node_id}/... previous versions
//	trash/{node_id}/...    deleted subtrees
//	uploads/               in-flight writes, renamed into place when done
//	previews/              generated thumbnails
package localfs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"syscall"

	"github.com/do-service/cloud/backend/internal/storage"
)

// Areas are the top-level directories of the layout.
var Areas = []string{"files", "versions", "trash", "uploads", "previews"}

const (
	dirPerm  = 0o750
	filePerm = 0o640
	// tmpDir holds Put/Copy temp files: inside the store, so the final
	// rename never crosses a filesystem boundary.
	tmpDir = "uploads"
)

type Store struct {
	// root confines every operation to the data dir, symlinks included,
	// as a second line of defence behind ValidateKey.
	root *os.Root
}

var _ storage.Backend = (*Store)(nil)

// Open prepares dir as a store: creates the layout and checks that all of
// it is one filesystem. rename(2) across filesystems fails (EXDEV), and
// every "atomic, O(1)" step of versions and trash relies on it, so a
// separately mounted versions/ or trash/ must fail startup, not the first
// upload.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open data dir: %w", err)
	}
	for _, a := range Areas {
		if err := root.MkdirAll(a, dirPerm); err != nil {
			root.Close()
			return nil, fmt.Errorf("create %s: %w", a, err)
		}
	}
	if err := checkSameFS(func(d string) (uint64, error) { return device(root, d) }, append([]string{"."}, Areas...)); err != nil {
		root.Close()
		return nil, err
	}
	return &Store{root: root}, nil
}

func (s *Store) Close() error { return s.root.Close() }

func (s *Store) Layout() storage.Layout { return storage.LayoutTree }

func device(root *os.Root, dir string) (uint64, error) {
	fi, err := root.Stat(dir)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no device information")
	}
	return uint64(st.Dev), nil
}

func checkSameFS(device func(string) (uint64, error), dirs []string) error {
	var want uint64
	for i, d := range dirs {
		dev, err := device(d)
		if err != nil {
			return fmt.Errorf("stat %s: %w", d, err)
		}
		if i == 0 {
			want = dev
		} else if dev != want {
			return fmt.Errorf("%s is on a different filesystem than the data dir: versions, trash and uploads are moved with rename(2), which only works within one filesystem", d)
		}
	}
	return nil
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) (storage.PutResult, error) {
	if err := storage.ValidateKey(key); err != nil {
		return storage.PutResult{}, err
	}
	h := sha256.New()
	var n int64
	err := s.writeAtomic(ctx, key, func(f *os.File) error {
		var err error
		n, err = io.Copy(io.MultiWriter(f, h), ctxReader{ctx, r})
		if err != nil {
			return err
		}
		if size >= 0 && n != size {
			return fmt.Errorf("%w: declared %d, got %d", storage.ErrSizeMismatch, size, n)
		}
		return nil
	})
	if err != nil {
		return storage.PutResult{}, err
	}
	res := storage.PutResult{Size: n}
	h.Sum(res.SHA256[:0])
	return res, nil
}

func (s *Store) Get(ctx context.Context, key string, rng *storage.Range) (io.ReadCloser, error) {
	if err := s.check(ctx, key); err != nil {
		return nil, err
	}
	f, err := s.root.Open(key)
	if err != nil {
		return nil, mapErr(err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, mapErr(err)
	}
	if fi.IsDir() {
		f.Close()
		return nil, fmt.Errorf("get %s: is a directory: %w", key, storage.ErrNotExist)
	}
	if rng == nil {
		return f, nil
	}
	if rng.Offset < 0 || rng.Offset > fi.Size() {
		f.Close()
		return nil, storage.ErrInvalidRange
	}
	if _, err := f.Seek(rng.Offset, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	if rng.Length < 0 {
		return f, nil
	}
	return limitedFile{Reader: io.LimitReader(f, rng.Length), Closer: f}, nil
}

func (s *Store) Stat(ctx context.Context, key string) (storage.Info, error) {
	if err := s.check(ctx, key); err != nil {
		return storage.Info{}, err
	}
	fi, err := s.root.Stat(key)
	if err != nil {
		return storage.Info{}, mapErr(err)
	}
	return storage.Info{Size: fi.Size(), ModTime: fi.ModTime(), IsDir: fi.IsDir()}, nil
}

func (s *Store) Mkdir(ctx context.Context, key string) error {
	if err := s.check(ctx, key); err != nil {
		return err
	}
	return mapErr(s.root.MkdirAll(key, dirPerm))
}

func (s *Store) Move(ctx context.Context, from, to string) error {
	if err := s.check(ctx, from); err != nil {
		return err
	}
	if err := s.check(ctx, to); err != nil {
		return err
	}
	src, err := s.root.Lstat(from)
	if err != nil {
		return mapErr(err)
	}
	// rename(2) would silently replace a file at to. Check-then-rename is
	// racy, but the fs service serializes writers of a subtree (E1-5), so
	// nothing else creates to in between. On a case-insensitive filesystem
	// (macOS dev machines) "a" and "A" are the same entry: that is a case
	// change, not an overwrite.
	if dst, err := s.root.Lstat(to); err == nil {
		if !os.SameFile(src, dst) {
			return fmt.Errorf("move to %s: %w", to, storage.ErrExist)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return mapErr(err)
	}
	if err := s.root.MkdirAll(path.Dir(to), dirPerm); err != nil {
		return mapErr(err)
	}
	if err := s.root.Rename(from, to); err != nil {
		return mapErr(err)
	}
	return s.syncDir(path.Dir(to))
}

func (s *Store) Copy(ctx context.Context, from, to string) error {
	if err := s.check(ctx, from); err != nil {
		return err
	}
	if err := storage.ValidateKey(to); err != nil {
		return err
	}
	src, err := s.root.Open(from)
	if err != nil {
		return mapErr(err)
	}
	defer src.Close()
	if fi, err := src.Stat(); err != nil {
		return mapErr(err)
	} else if fi.IsDir() {
		return fmt.Errorf("copy %s: is a directory: %w", from, storage.ErrNotExist)
	}
	return s.writeAtomic(ctx, to, func(f *os.File) error {
		// *os.File to *os.File: io.Copy uses copy_file_range on Linux,
		// which shares extents on Btrfs/XFS instead of copying bytes.
		_, err := io.Copy(f, src)
		return err
	})
}

func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.check(ctx, key); err != nil {
		return err
	}
	if err := s.root.RemoveAll(key); err != nil {
		return mapErr(err)
	}
	return nil
}

func (s *Store) check(ctx context.Context, key string) error {
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	return ctx.Err()
}

// writeAtomic has write fill a temp file in uploads/, fsyncs it and renames
// it over key, so a crash leaves either the old content or the new one.
// On failure the temp file is removed; one orphaned by a crash is left for
// the upload cleanup job (E3-5).
func (s *Store) writeAtomic(ctx context.Context, key string, write func(*os.File) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.root.MkdirAll(path.Dir(key), dirPerm); err != nil {
		return mapErr(err)
	}
	tmp := path.Join(tmpDir, ".put-"+randomSuffix())
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return mapErr(err)
	}
	defer func() {
		if err != nil {
			f.Close()
			_ = s.root.Remove(tmp)
		}
	}()
	if err := write(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := s.root.Rename(tmp, key); err != nil {
		return mapErr(err)
	}
	return s.syncDir(path.Dir(key))
}

// syncDir makes a rename into dir durable: without it the new directory
// entry can be lost on power failure even though the file data was synced.
func (s *Store) syncDir(dir string) error {
	d, err := s.root.Open(dir)
	if err != nil {
		return mapErr(err)
	}
	return errors.Join(d.Sync(), d.Close())
}

func randomSuffix() string {
	var b [12]byte
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// mapErr translates OS errors into storage's sentinels, keeping the
// original in the chain for logs.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %w", storage.ErrNotExist, err)
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%w: %w", storage.ErrExist, err)
	}
	return err
}

// ctxReader stops a long copy once the request is cancelled; plain file
// I/O doesn't look at the context by itself.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type limitedFile struct {
	io.Reader
	io.Closer
}
