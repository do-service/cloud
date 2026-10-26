// Package watch reports changes under a local folder (docs/desktop-client.md):
// FSEvents on macOS, inotify (through fsnotify) on Linux, one recursive
// ReadDirectoryChangesW on the root on Windows. Not fsnotify on Windows:
// it opens a handle per folder, and an open handle on a subfolder stops
// the user from renaming its parent. Not
// fsnotify on macOS either: there it is kqueue, one file descriptor per
// file and no recursion, which a sync folder of 100 000 files can't
// afford; FSEvents is one stream per tree and can also replay what
// changed while the client wasn't running, from a saved event id.
//
// Events are hints, not a log: the OS coalesces them (a file created and
// deleted within the latency window arrives as one event with both ops),
// may drop them under load (MustRescan) and says nothing about content. The
// engine treats an event as "look at this path again" and rescans on
// MustRescan.
package watch

import (
	"errors"
	"strings"
	"time"
)

// Op is what happened to a path; several bits at once are normal.
type Op uint32

const (
	Create Op = 1 << iota
	Remove
	// Rename is reported for both the old and the new path, unpaired: the
	// one that still exists is the new name.
	Rename
	Modify
	// Meta covers mode, owner, xattrs and Finder info.
	Meta
	IsDir
	IsFile
	IsSymlink
	// MustRescan: events under Path were lost (the OS or this process fell
	// behind); only a scan of Path tells what changed.
	MustRescan
	// HistoryDone marks the end of the replay of events since the id the
	// watcher was started from; what follows is live.
	HistoryDone
	// RootChanged: the watched folder itself was moved, deleted or replaced.
	RootChanged
)

var opNames = []struct {
	op   Op
	name string
}{
	{Create, "create"},
	{Remove, "remove"},
	{Rename, "rename"},
	{Modify, "modify"},
	{Meta, "meta"},
	{IsDir, "dir"},
	{IsFile, "file"},
	{IsSymlink, "symlink"},
	{MustRescan, "rescan"},
	{HistoryDone, "history-done"},
	{RootChanged, "root-changed"},
}

func (o Op) Has(bit Op) bool { return o&bit != 0 }

func (o Op) String() string {
	var parts []string
	for _, n := range opNames {
		if o.Has(n.op) {
			parts = append(parts, n.name)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "|")
}

// Event is one changed path. Path is absolute and as the OS reports it:
// on macOS with symlinks resolved (/private/var/…, not /var/…), so match
// it against Watcher.Root, not against the path given to New.
type Event struct {
	Path string
	// ID orders events and is what to persist: starting a watcher from the
	// last handled ID replays everything after it (macOS only; 0 elsewhere).
	ID uint64
	Op Op
	// Inode is the file id at the time of the event, 0 when unknown. It is
	// how a rename is told apart from a delete plus an unrelated create.
	Inode uint64
}

// Watcher delivers the events under one folder, recursively, in batches.
// New returns the one this platform has.
type Watcher interface {
	// Root is the watched folder with symlinks resolved, the prefix of
	// every Event.Path.
	Root() string
	// Events yields batches until Close; it is closed then.
	Events() <-chan []Event
	// Close stops watching. Batches not yet received are dropped: the
	// caller resumes from the last ID it handled, or rescans.
	Close() error
}

// SinceNow starts a watcher with live events only, no replay.
const SinceNow = ^uint64(0)

// Options for New.
type Options struct {
	// Since is the last event id already handled, or SinceNow.
	Since uint64
	// Latency is how long the OS gathers events before delivering a batch;
	// longer means fewer, more coalesced batches. Zero means 200 ms.
	Latency time.Duration
}

// ErrUnsupported is returned by New where there's no native watcher (or
// on macOS when built without cgo, which FSEvents needs); the engine falls
// back to periodic rescans.
var ErrUnsupported = errors.New("watch: not supported on this platform")
