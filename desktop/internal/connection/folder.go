package connection

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// Choosing a sync folder: when a cloud is added (Setup) and when its
// folder is changed (MoveFolder).

var (
	// ErrNotFolder: the path is a file (or a link), not a folder.
	ErrNotFolder = errors.New("connection: not a folder")
	// ErrFolderNotEmpty: a folder to move the sync folder to has files.
	ErrFolderNotEmpty = errors.New("connection: the folder is not empty")
	// ErrFolderTaken: the folder is the sync folder of another
	// connection, or was one of another server or account.
	ErrFolderTaken = errors.New("connection: the folder belongs to another connection")
	// ErrInsideCurrent: the new place is the sync folder itself or inside it.
	ErrInsideCurrent = errors.New("connection: the new folder is inside the current one")
	// ErrCrossVolume: the new place is on another disk. A move there would
	// be a copy, which is refused: see MoveFolder.
	ErrCrossVolume = errors.New("connection: the folder is on another disk")
)

// countLimit caps Folder.Files: the window only says "more than".
const countLimit = 10000

// Folder is a folder chosen to be a sync folder, as checked.
type Folder struct {
	// Path is absolute and clean.
	Path string `json:"path"`
	// Exists: there is a folder at Path; else it will be created.
	Exists bool `json:"exists"`
	// Files and Folders found in it (marker and .DS_Store aside), up to
	// countLimit together; More when there are more. A folder with any is
	// adopted (see Setup).
	Files   int  `json:"files"`
	Folders int  `json:"folders"`
	More    bool `json:"more"`
	// Replace: the folder has the marker of a connection gone from this
	// computer (an earlier install) of the same account, or of unknown
	// account (an older client's marker): it is taken over.
	Replace bool `json:"replace"`
}

// Empty: nothing to adopt.
func (f Folder) Empty() bool { return f.Files == 0 && f.Folders == 0 }

// CheckFolder checks dir as the sync folder of a new connection to
// server for user userID ("" before the sign-in, when only the server is
// known): an absolute folder or nothing yet at dir, not nested with
// another connection's folder or the state directory (state.ErrNested),
// and not another connection's (ErrFolderTaken). Files in it are
// allowed: Setup adopts them.
func CheckFolder(base, server, userID, dir string) (Folder, error) {
	f, err := inspect(base, "", dir)
	if err != nil {
		return f, err
	}
	m, err := localfs.ReadMarker(f.Path)
	if errors.Is(err, localfs.ErrNoMarker) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	conns, err := state.OpenRegistry(base).List()
	if err != nil {
		return f, err
	}
	for _, c := range conns {
		if c.ID == m.ConnID {
			// Registered: its account may connect it again (Setup finds
			// the connection), nobody else.
			if c.Server != server || (userID != "" && c.UserID != userID) {
				return f, fmt.Errorf("%w: %s", ErrFolderTaken, f.Path)
			}
			return f, nil
		}
	}
	if (m.Server != "" && m.Server != server) || (m.UserID != "" && userID != "" && m.UserID != userID) {
		return f, fmt.Errorf("%w: %s", ErrFolderTaken, f.Path)
	}
	f.Replace = true
	return f, nil
}

// CheckMoveTarget checks dir as the new place of conn's sync folder: as
// CheckFolder, and also empty (only .DS_Store, which Finder leaves) or
// not there yet — moving into files would merge two trees — and on the
// same disk as the current folder.
func CheckMoveTarget(base string, conn state.Connection, dir string) (Folder, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Folder{}, err
	}
	// Around it is refused below: not empty.
	if state.Nested(conn.LocalRoot, abs, foldCase()) && len(abs) >= len(filepath.Clean(conn.LocalRoot)) {
		return Folder{Path: abs}, fmt.Errorf("%w: %s", ErrInsideCurrent, abs)
	}
	f, err := inspect(base, conn.ID, abs)
	if err != nil {
		return f, err
	}
	// Strictly empty: what the engine ignores (a marker, Thumbs.db, a
	// .tmp) would be deleted with the folder the sync folder replaces.
	if f.Exists {
		ents, err := os.ReadDir(f.Path)
		if err != nil {
			return f, err
		}
		for _, e := range ents {
			if e.Name() != ".DS_Store" {
				return f, fmt.Errorf("%w: %s", ErrFolderNotEmpty, f.Path)
			}
		}
	}
	from, err := localfs.VolumeOf(conn.LocalRoot)
	if err != nil {
		return f, err
	}
	to, err := localfs.VolumeOf(existingAncestor(f.Path))
	if err != nil {
		return f, err
	}
	if from != to {
		return f, fmt.Errorf("%w: %s", ErrCrossVolume, f.Path)
	}
	return f, nil
}

// inspect is the checks shared by a new folder and a move: the path, the
// nesting, and what is in it.
func inspect(base, connID, dir string) (Folder, error) {
	if dir == "" {
		return Folder{}, fmt.Errorf("%w: no path", ErrNotFolder)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Folder{}, err
	}
	f := Folder{Path: filepath.Clean(abs)}
	if err := state.OpenRegistry(base).CheckRoot(connID, f.Path); err != nil {
		return f, err
	}
	// Lstat: a link would be followed by the engine's scan but not by a
	// rename onto it; a real folder only.
	fi, err := os.Lstat(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if !fi.IsDir() {
		return f, fmt.Errorf("%w: %s", ErrNotFolder, f.Path)
	}
	f.Exists = true
	err = filepath.WalkDir(f.Path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == f.Path {
			return nil // unreadable: the engine's scan reports it
		}
		// What the engine never syncs (.DS_Store, the marker) doesn't count.
		if localfs.Ignored(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if f.Files+f.Folders >= countLimit {
			f.More = true
			return fs.SkipAll
		}
		if d.IsDir() {
			f.Folders++
		} else {
			f.Files++
		}
		return nil
	})
	return f, err
}

// topLevel lists what a folder being adopted holds at its top, the
// marker and what the engine never syncs aside.
func topLevel(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !localfs.Ignored(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// foldCase: names compared as the default file systems of macOS and
// Windows do, as state.Registry does.
func foldCase() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "windows" }

func existingAncestor(p string) string {
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		up := filepath.Dir(p)
		if up == p {
			return p
		}
		p = up
	}
}
