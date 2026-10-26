package localfs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

var (
	// ErrHashMismatch: the downloaded content isn't what the server said;
	// the .part file is deleted, the download starts over.
	ErrHashMismatch = errors.New("localfs: downloaded content doesn't match its sha256")
	// ErrExists: the target of a rename is another file or folder.
	ErrExists = errors.New("localfs: target exists")
	// ErrNotEmpty: a folder to remove still has something besides OS junk.
	ErrNotEmpty = errors.New("localfs: folder not empty")
)

// PartPath is where a download of rel goes before it's complete:
// .~cloud-<id>.part in the same folder, so the final rename stays on one
// volume (atomic) and the scan ignores it. id is normally the node id,
// which keeps the name stable across restarts for resuming with Range.
func PartPath(root, rel, id string) (string, error) {
	if !validRel(rel) || id == "" || strings.ContainsAny(id, `/\:`) {
		return "", fmt.Errorf("localfs: bad part for %q, %q", rel, id)
	}
	return filepath.Join(root, filepath.FromSlash(path.Dir(rel)), tempPrefix+id+".part"), nil
}

// OpenPart opens a part file for appending, creating it if needed, and
// returns its size: the offset to resume the download from.
func OpenPart(part string) (*os.File, int64, error) {
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// CommitOptions for Commit.
type CommitOptions struct {
	// SHA256 is the content's expected hash (hex); "" skips the check.
	SHA256 string
	// MTime is set on the file (the server's mtime); zero leaves it.
	MTime time.Time
	// Expect is the target's stamp at the last sync (base), when it is
	// replaced; nil when the file is new and the target must not exist.
	Expect *Stamp
	// Sync, when set, takes the folder's flush (Syncer.Wait before the
	// result is recorded); nil flushes it before Commit returns.
	Sync *Syncer
}

// Commit puts a complete part file in place of root/rel. In order: the
// part's hash is checked, its mtime set, it is flushed to disk ahead of
// the rename (durable.go), the target
// re-checked against opts.Expect so a local edit made since the last sync
// is never overwritten (ErrChanged, the part is kept for the caller to
// place elsewhere), then rename and a flush of the folder. The user's file
// is never written in place: a crash leaves the old file or the new one.
//
// The re-check and the rename are two steps: an edit landing between
// them is lost to the new version. The window is microseconds, against
// seconds for a download.
//
// The returned entry is the file as it now is on disk, its mtime possibly
// coarser than opts.MTime (FAT: 2 s): what to keep in base.
func Commit(root, rel, part string, opts CommitOptions) (Entry, error) {
	if !validRel(rel) {
		return Entry{}, fmt.Errorf("localfs: bad relative path %q", rel)
	}
	if opts.SHA256 != "" {
		sum, err := hashFile(part)
		if err != nil {
			return Entry{}, err
		}
		if !strings.EqualFold(sum, opts.SHA256) {
			os.Remove(part)
			return Entry{}, fmt.Errorf("%w: %s", ErrHashMismatch, rel)
		}
	}
	if !opts.MTime.IsZero() {
		// A zero atime leaves it as it is.
		if err := os.Chtimes(part, time.Time{}, opts.MTime); err != nil {
			return Entry{}, err
		}
	}
	if err := flushFile(part); err != nil {
		return Entry{}, err
	}
	if err := ordered(opts.Sync, filepath.Dir(part)); err != nil {
		return Entry{}, err
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	cur, err := stat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if opts.Expect != nil {
			return Entry{}, fmt.Errorf("%w: %s was deleted locally", ErrChanged, rel)
		}
	case err != nil:
		return Entry{}, err
	case opts.Expect == nil:
		return Entry{}, fmt.Errorf("%w: %s appeared locally", ErrChanged, rel)
	case !cur.Same(*opts.Expect):
		return Entry{}, fmt.Errorf("%w: %s was edited locally", ErrChanged, rel)
	}
	if err := os.Rename(part, target); err != nil {
		return Entry{}, err
	}
	if err := dirSynced(opts.Sync, filepath.Dir(target)); err != nil {
		return Entry{}, err
	}
	st, err := stat(target)
	if err != nil {
		return Entry{}, err
	}
	return Entry{RelPath: rel, Stamp: st}, nil
}

// flushFile moves name's data to the device (fileSync); ordered then
// keeps it ahead of the rename.
func flushFile(name string) error {
	f, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := fileSync(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// WriteFile is a whole download in one call: r into a fresh part file,
// then Commit. For small files and tests; big ones go through OpenPart to
// resume.
func WriteFile(root, rel, id string, r io.Reader, opts CommitOptions) (Entry, error) {
	part, err := PartPath(root, rel, id)
	if err != nil {
		return Entry{}, err
	}
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return Entry{}, err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return Entry{}, err
	}
	e, err := Commit(root, rel, part, opts)
	if err != nil && !errors.Is(err, ErrChanged) {
		os.Remove(part)
	}
	return e, err
}

// Rename moves root/from to root/to, refusing to replace anything there
// (ErrExists); the destination folder must exist. A rename that changes
// only case or Unicode normalization goes through a temporary name: on a
// case-insensitive disk the target "exists" (it is the source), and some
// file systems treat such a rename as a no-op. The temporary name is
// derived from both paths, so after a crash between the two steps calling
// Rename again finishes the job — the engine redoes it from its op journal,
// before trusting a scan, which ignores the temporary name and so would
// see the file as deleted.
func Rename(root, from, to string) error { return rename(root, from, to, nil) }

func rename(root, from, to string, s *Syncer) error {
	if !validRel(from) || !validRel(to) {
		return fmt.Errorf("localfs: bad rename %q → %q", from, to)
	}
	if from == to {
		return nil
	}
	src := filepath.Join(root, filepath.FromSlash(from))
	dst := filepath.Join(root, filepath.FromSlash(to))
	sameName := path.Dir(from) == path.Dir(to) && NameKey(path.Base(from)) == NameKey(path.Base(to))
	if !sameName {
		if _, err := os.Lstat(dst); err == nil {
			return fmt.Errorf("%w: %s", ErrExists, to)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
		return dirSynced(s, filepath.Dir(dst), filepath.Dir(src))
	}

	sum := sha256.Sum256([]byte(from + "\x00" + to))
	tmp := filepath.Join(filepath.Dir(src), tempPrefix+"rename-"+hex.EncodeToString(sum[:8]))
	srcInfo, err := os.Lstat(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Crashed after the first step: the file is at tmp.
		if _, terr := os.Lstat(tmp); terr != nil {
			return err
		}
	case err != nil:
		return err
	default:
		// On a case-sensitive disk "to" may be a different file.
		if dstInfo, err := os.Lstat(dst); err == nil && !os.SameFile(srcInfo, dstInfo) {
			return fmt.Errorf("%w: %s", ErrExists, to)
		}
		if err := os.Rename(src, tmp); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	return dirSynced(s, filepath.Dir(dst))
}

// Remove deletes root/rel if it is still what the caller last synced
// (expect, from base): an edit made since is not deleted (ErrChanged). A
// folder must be empty but for OS junk (.DS_Store, Thumbs.db…), which goes
// with it; the caller removes the synced contents first, each with its
// own expectation. Already gone is success.
func Remove(root, rel string, expect Stamp) error { return remove(root, rel, expect, nil) }

func remove(root, rel string, expect Stamp, s *Syncer) error {
	if !validRel(rel) {
		return fmt.Errorf("localfs: bad relative path %q", rel)
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	cur, err := stat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !cur.Same(expect) {
		return fmt.Errorf("%w: %s", ErrChanged, rel)
	}
	if cur.Kind == KindDir {
		ents, err := os.ReadDir(abs)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if !junk(e.Name()) {
				return fmt.Errorf("%w: %s", ErrNotEmpty, rel)
			}
		}
		for _, e := range ents {
			if err := os.Remove(filepath.Join(abs, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
		if cur.Kind == KindDir {
			// Something appeared since the listing.
			return fmt.Errorf("%w: %s: %v", ErrNotEmpty, rel, err)
		}
		return err
	}
	return dirSynced(s, filepath.Dir(abs))
}
