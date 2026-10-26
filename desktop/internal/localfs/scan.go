// Package localfs is the sync client's side of the local disk
// (docs/desktop-client.md, «Движок»): scanning a sync folder with stat
// only, names as the server compares them, what to ignore, file hashes
// cached by stat, and writes that never leave a user's file half-written
// or overwrite an edit made since the last sync.
//
// Paths given to and returned by this package are relative to the sync
// folder, slash-separated, with names exactly as the file system returned
// them (on macOS they may be NFD); compare them with NameKey, not ==.
package localfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Kind of an entry, named as the server names it.
type Kind string

const (
	KindDir  Kind = "dir"
	KindFile Kind = "file"
)

// Stamp is what a stat says about an entry: equal stamps mean nothing
// changed, without reading the content.
type Stamp struct {
	Kind    Kind
	Size    int64
	MtimeNS int64
	FileID  FileID
}

// Same compares two stamps. A folder's size and mtime change with its
// entries, which sync tracks one by one, so for folders only the id
// counts.
func (s Stamp) Same(o Stamp) bool {
	if s.Kind != o.Kind || !s.FileID.Matches(o.FileID) {
		return false
	}
	return s.Kind == KindDir || (s.Size == o.Size && s.MtimeNS == o.MtimeNS)
}

// Entry is a file or folder found by Scan.
type Entry struct {
	RelPath string
	Stamp
}

// SkipReason says why Scan left an entry out of sync and wants the user
// told; ignored names are left out silently.
type SkipReason string

const (
	SkipSymlink SkipReason = "symlink"
	// SkipSpecial: a device, socket, pipe, or a Windows reparse point that
	// is not a plain file (junction, cloud placeholder).
	SkipSpecial SkipReason = "special"
	// SkipMount: a folder on another volume, as a disk mounted inside the
	// sync folder; syncing it would upload the whole disk.
	SkipMount SkipReason = "mount"
	// SkipUnreadable: a folder that can't be listed or an entry that can't
	// be stat'ed; Err says why.
	SkipUnreadable SkipReason = "unreadable"
)

type Skipped struct {
	RelPath string
	Reason  SkipReason
	Err     error
}

// Result of a scan. Entries are in depth-first order, each folder before
// its contents, siblings by name as bytes.
type Result struct {
	Entries []Entry
	Skipped []Skipped
}

// Scan lists sub and everything below it in the sync folder root; sub ""
// is the whole folder (not itself an entry). A partial scan of a subtree
// is how a watcher event is followed up: whatever the previous scan saw
// under sub is replaced by this result. A missing sub gives an empty
// result: it's gone. So does an ignored one, or one under an ignored
// folder; one at or under a symlink is reported skipped.
func Scan(root, sub string) (Result, error) {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return Result{}, fmt.Errorf("localfs: scan: %w", err)
	}
	if !rootInfo.IsDir() {
		return Result{}, fmt.Errorf("localfs: scan: %s is not a folder", root)
	}
	rootID, err := fileID(root, rootInfo)
	if err != nil {
		return Result{}, fmt.Errorf("localfs: scan: %w", err)
	}
	s := &scanner{root: root, volume: rootID.Volume}
	if sub == "" {
		s.walk("")
		return s.res, nil
	}
	if !validRel(sub) {
		return Result{}, fmt.Errorf("localfs: scan: bad relative path %q", sub)
	}
	// Each step down to sub must be a real folder: a symlink on the way
	// would lead the scan out of the sync folder.
	parts := strings.Split(sub, "/")
	for i, name := range parts {
		rel := strings.Join(parts[:i+1], "/")
		if Ignored(name) {
			return s.res, nil
		}
		fi, err := os.Lstat(s.abs(rel))
		if errors.Is(err, fs.ErrNotExist) {
			return s.res, nil
		}
		if err != nil {
			s.skip(rel, SkipUnreadable, err)
			return s.res, nil
		}
		if i == len(parts)-1 {
			s.visit(rel, fi)
		} else if fi.Mode()&fs.ModeSymlink != 0 {
			s.skip(rel, SkipSymlink, nil)
			return s.res, nil
		} else if !fi.IsDir() {
			// A file where sub expects a folder: sub can't exist.
			return s.res, nil
		}
	}
	return s.res, nil
}

type scanner struct {
	root   string
	volume uint64
	res    Result
}

func (s *scanner) abs(rel string) string {
	return filepath.Join(s.root, filepath.FromSlash(rel))
}

func (s *scanner) skip(rel string, why SkipReason, err error) {
	s.res.Skipped = append(s.res.Skipped, Skipped{RelPath: rel, Reason: why, Err: err})
}

// visit records one entry and, for a folder, everything in it.
func (s *scanner) visit(rel string, fi fs.FileInfo) {
	mode := fi.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		s.skip(rel, SkipSymlink, nil)
		return
	case !mode.IsDir() && !mode.IsRegular():
		s.skip(rel, SkipSpecial, nil)
		return
	}
	id, err := fileID(s.abs(rel), fi)
	if errors.Is(err, fs.ErrNotExist) {
		return // deleted since the listing
	}
	if err != nil {
		s.skip(rel, SkipUnreadable, err)
		return
	}
	if mode.IsDir() && id.Volume != s.volume {
		s.skip(rel, SkipMount, nil)
		return
	}
	e := Entry{RelPath: rel, Stamp: Stamp{Kind: KindFile, Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano(), FileID: id}}
	if mode.IsDir() {
		e.Kind, e.Size = KindDir, 0
	}
	s.res.Entries = append(s.res.Entries, e)
	if mode.IsDir() {
		s.walk(rel)
	}
}

func (s *scanner) walk(dir string) {
	ents, err := os.ReadDir(s.abs(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		if dir == "" {
			dir = "."
		}
		s.skip(dir, SkipUnreadable, err)
		return
	}
	for _, d := range ents {
		if Ignored(d.Name()) {
			continue
		}
		rel := path.Join(dir, d.Name())
		// Info is lstat (fstatat): the size, mtime and type without
		// following a symlink.
		fi, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			s.skip(rel, SkipUnreadable, err)
			continue
		}
		s.visit(rel, fi)
	}
}

// validRel accepts a clean relative slash path: no leading or trailing
// '/', no empty, "." or ".." component.
func validRel(rel string) bool {
	if rel == "" || strings.Contains(rel, `\`) && filepath.Separator == '\\' {
		return false
	}
	for _, p := range strings.Split(rel, "/") {
		if p == "" || p == "." || p == ".." {
			return false
		}
	}
	return true
}
