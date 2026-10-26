package localfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrNoMarker: the sync folder has no .cloudsync, or is gone — an
	// unplugged disk, a folder moved away. Sync must stop: the empty mount
	// point would otherwise look like every file deleted.
	ErrNoMarker = errors.New("localfs: sync folder marker missing")
	// ErrMarkerMismatch: the folder is another connection's sync folder.
	ErrMarkerMismatch = errors.New("localfs: sync folder belongs to another connection")
)

// Marker is what .cloudsync says: the connection's id on its first line,
// then the server and the user, so a folder found again after the
// client's state is gone (a reinstall) tells whose it was. Markers of
// older clients have the id only.
type Marker struct {
	ConnID string
	Server string
	UserID string
}

// CreateMarker makes root a sync folder of connection connID: writes
// root/.cloudsync with the id. Idempotent for the same connection;
// ErrMarkerMismatch if another one owns the folder.
func CreateMarker(root, connID string) error {
	return createMarker(root, Marker{ConnID: connID}, false)
}

// WriteMarker is CreateMarker with the server and user in the marker;
// replace lets it take over a folder with another connection's marker,
// which the caller has found to be a gone connection of the same account.
func WriteMarker(root string, m Marker, replace bool) error {
	return createMarker(root, m, replace)
}

func createMarker(root string, m Marker, replace bool) error {
	err := CheckMarker(root, m.ConnID)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrMarkerMismatch) && replace:
	case !errors.Is(err, ErrNoMarker):
		return err
	}
	body := m.ConnID + "\n"
	if m.Server != "" {
		body += "server " + m.Server + "\n"
	}
	if m.UserID != "" {
		body += "user " + m.UserID + "\n"
	}
	// Written aside and renamed over: the folder is never without a
	// whole marker, which would stop sync (or, replacing, briefly be
	// another connection's half-written one).
	f, err := os.CreateTemp(root, tempPrefix+"marker-*")
	if err != nil {
		return fmt.Errorf("localfs: marker: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(body); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(root, MarkerName))
	}
	if err != nil {
		return fmt.Errorf("localfs: marker: %w", err)
	}
	return syncDir(root)
}

// ReadMarker reads root's marker: ErrNoMarker without one.
func ReadMarker(root string) (Marker, error) {
	raw, err := os.ReadFile(filepath.Join(root, MarkerName))
	if errors.Is(err, fs.ErrNotExist) {
		return Marker{}, fmt.Errorf("%w: %s", ErrNoMarker, root)
	}
	if err != nil {
		return Marker{}, fmt.Errorf("localfs: marker: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	m := Marker{ConnID: strings.TrimSpace(lines[0])}
	for _, l := range lines[1:] {
		k, v, _ := strings.Cut(strings.TrimSpace(l), " ")
		switch k {
		case "server":
			m.Server = v
		case "user":
			m.UserID = v
		}
	}
	return m, nil
}

// CheckMarker is the check before every sync pass: nil only if root has
// the marker of connID.
func CheckMarker(root, connID string) error {
	m, err := ReadMarker(root)
	if err != nil {
		return err
	}
	if m.ConnID != connID {
		return fmt.Errorf("%w: %s", ErrMarkerMismatch, root)
	}
	return nil
}

// VolumeOf is the device (volume) path is on, to tell whether a rename
// to another place can work: renames don't cross volumes.
func VolumeOf(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	id, err := fileID(path, fi)
	return id.Volume, err
}
