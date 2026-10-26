package localfs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrChanged: the file changed while being read, or since the state the
// caller expected. Try again later (it is probably still being written)
// or re-plan.
var ErrChanged = errors.New("localfs: file changed")

// Hashed is a file's SHA-256 (hex) together with the stamp it was
// computed for: valid as long as a new stat gives the same stamp.
type Hashed struct {
	Stamp
	SHA256 string
}

// Hash returns the hash of root/rel, reusing prev when the file's stamp is
// still prev's (size, mtime and file id unchanged), so a rescan of a big
// folder reads no content. Otherwise it reads the file and checks the
// stamp didn't change meanwhile (ErrChanged if it did).
func Hash(root, rel string, prev Hashed) (Hashed, error) {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	before, err := stat(abs)
	if err != nil {
		return Hashed{}, err
	}
	if before.Kind != KindFile {
		return Hashed{}, fmt.Errorf("localfs: hash: %s is a folder", rel)
	}
	if prev.SHA256 != "" && prev.Stamp.Same(before) {
		return prev, nil
	}
	sum, err := hashFile(abs)
	if err != nil {
		return Hashed{}, err
	}
	after, err := stat(abs)
	if err != nil {
		return Hashed{}, err
	}
	if !after.Same(before) {
		return Hashed{}, fmt.Errorf("%w: %s while hashing", ErrChanged, rel)
	}
	return Hashed{Stamp: after, SHA256: sum}, nil
}

func hashFile(abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("localfs: hash %s: %w", abs, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// stat is lstat as a Stamp. A symlink or special file is an error: sync
// never touches them.
func stat(abs string) (Stamp, error) {
	fi, err := os.Lstat(abs)
	if err != nil {
		return Stamp{}, err
	}
	if !fi.Mode().IsDir() && !fi.Mode().IsRegular() {
		return Stamp{}, fmt.Errorf("localfs: %s is not a file or folder", abs)
	}
	id, err := fileID(abs, fi)
	if err != nil {
		return Stamp{}, err
	}
	if fi.IsDir() {
		return Stamp{Kind: KindDir, MtimeNS: fi.ModTime().UnixNano(), FileID: id}, nil
	}
	return Stamp{Kind: KindFile, Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano(), FileID: id}, nil
}

// Stat returns the stamp of root/rel, the thing to keep in base after a
// sync and to pass back as an expectation to Commit and Remove.
func Stat(root, rel string) (Stamp, error) {
	return stat(filepath.Join(root, filepath.FromSlash(rel)))
}
