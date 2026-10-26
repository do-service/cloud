package state

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// FileName is the database's name inside a connection's directory.
const FileName = "state.db"

// BaseDir is where the client keeps its state: outside any sync folder,
// in the per-user location each OS has for application state. It holds
// the connections registry and one directory per connection.
//
//	macOS    ~/Library/Application Support/Cloud
//	Windows  %LOCALAPPDATA%\Cloud
//	other    $XDG_STATE_HOME/cloud (~/.local/state by default)
//
// It doesn't create the directory.
func BaseDir() (string, error) {
	home, _ := os.UserHomeDir()
	return baseDirFor(runtime.GOOS, os.Getenv, home)
}

// Dir is BaseDir/<connection id>, the directory of a connection's state.
func Dir(connID string) (string, error) {
	if !validID(connID) {
		return "", errors.New("state: connection id must be one path component")
	}
	base, err := BaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, connID), nil
}

// Path is Dir(connID)/state.db.
func Path(connID string) (string, error) {
	dir, err := Dir(connID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

func validID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\:`)
}

// baseDirFor builds the path with the separators of goos, not of the OS
// running it, so the tests cover all three.
func baseDirFor(goos string, getenv func(string) string, home string) (string, error) {
	switch goos {
	case "darwin":
		if home == "" {
			return "", errors.New("state: no home directory")
		}
		return home + "/Library/Application Support/Cloud", nil
	case "windows":
		base := getenv("LOCALAPPDATA")
		if base == "" {
			if home == "" {
				return "", errors.New("state: neither %LOCALAPPDATA% nor a home directory")
			}
			base = home + `\AppData\Local`
		}
		return base + `\Cloud`, nil
	default:
		// The XDG spec says to ignore a relative $XDG_STATE_HOME.
		base := getenv("XDG_STATE_HOME")
		if !strings.HasPrefix(base, "/") {
			if home == "" {
				return "", errors.New("state: neither $XDG_STATE_HOME nor a home directory")
			}
			base = home + "/.local/state"
		}
		return base + "/cloud", nil
	}
}
