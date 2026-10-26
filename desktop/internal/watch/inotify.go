//go:build linux

package watch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// inotify is the Watcher on Linux. inotify isn't recursive: every folder
// gets its own watch, added at start and whenever a folder appears. What
// lands in a new folder before its watch is added is not reported; the
// folder's own Create event is, and the engine scans a created folder's
// subtree anyway (a folder moved in brings no events for its contents on
// any platform).
type inotify struct {
	root    string
	fsw     *fsnotify.Watcher
	latency time.Duration
	events  chan []Event
	done    chan struct{}
	stopped chan struct{}
	once    sync.Once
	// dirs are the watched folders; only the loop goroutine touches it
	// after New returns.
	dirs map[string]struct{}
}

// New watches dir and everything below it. Options.Since is ignored:
// inotify has no history, so events while the client wasn't running are
// for the start-up scan to find.
func New(dir string, opts Options) (Watcher, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("watch: %w", err)
	}
	if root, err = filepath.Abs(root); err != nil {
		return nil, fmt.Errorf("watch: %w", err)
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watch: %w", err)
	}
	latency := opts.Latency
	if latency == 0 {
		latency = 200 * time.Millisecond
	}
	w := &inotify{
		root: root, fsw: fsw, latency: latency, dirs: map[string]struct{}{},
		events: make(chan []Event, 16), done: make(chan struct{}), stopped: make(chan struct{}),
	}
	// A partial watch would miss changes silently; better to fail and let
	// the engine fall back to periodic rescans. ENOSPC here is the
	// fs.inotify.max_user_watches limit.
	if err := w.addTree(root); err != nil {
		fsw.Close()
		if errors.Is(err, syscall.ENOSPC) {
			return nil, fmt.Errorf("watch: too many folders for inotify (raise fs.inotify.max_user_watches): %w", err)
		}
		return nil, fmt.Errorf("watch: %w", err)
	}
	go w.loop()
	return w, nil
}

func (w *inotify) Root() string { return w.root }

func (w *inotify) Events() <-chan []Event { return w.events }

func (w *inotify) Close() error {
	w.once.Do(func() {
		close(w.done)
		<-w.stopped
		w.fsw.Close()
		close(w.events)
	})
	return nil
}

// addTree watches dir and every folder below it, not following symlinks.
func (w *inotify) addTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p != dir {
				return nil // deleted while walking
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if err := w.fsw.Add(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) && p != dir {
				return filepath.SkipDir
			}
			return err
		}
		w.dirs[p] = struct{}{}
		return nil
	})
}

// forget drops the watches of a folder gone from p and below: a moved
// folder's watch would otherwise keep reporting under its old path.
func (w *inotify) forget(p string) bool {
	if _, ok := w.dirs[p]; !ok {
		return false
	}
	prefix := p + string(filepath.Separator)
	for d := range w.dirs {
		if d == p || strings.HasPrefix(d, prefix) {
			w.fsw.Remove(d) // fails when the kernel already dropped it
			delete(w.dirs, d)
		}
	}
	return true
}

func (w *inotify) loop() {
	defer close(w.stopped)
	var batch []Event
	var flush <-chan time.Time
	add := func(evs ...Event) {
		batch = append(batch, evs...)
		if flush == nil {
			flush = time.After(w.latency)
		}
	}
	for {
		select {
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			add(w.translate(ev))
		case _, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			// Overflow (fs.inotify.max_queued_events) or anything else:
			// what was lost is unknown, so the whole tree is suspect.
			add(Event{Path: w.root, Op: MustRescan})
		case <-flush:
			select {
			case w.events <- batch:
			case <-w.done:
				return
			}
			batch, flush = nil, nil
		case <-w.done:
			return
		}
	}
}

func (w *inotify) translate(ev fsnotify.Event) Event {
	e := Event{Path: ev.Name}
	for _, m := range []struct {
		in  fsnotify.Op
		out Op
	}{
		{fsnotify.Create, Create}, {fsnotify.Remove, Remove}, {fsnotify.Rename, Rename},
		{fsnotify.Write, Modify}, {fsnotify.Chmod, Meta},
	} {
		if ev.Has(m.in) {
			e.Op |= m.out
		}
	}
	if e.Path == w.root && e.Op.Has(Remove|Rename) {
		e.Op |= RootChanged
		return e
	}
	if e.Op.Has(Remove|Rename) && w.forget(e.Path) {
		e.Op |= IsDir
	}
	fi, err := os.Lstat(e.Path)
	if err != nil {
		return e
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		e.Inode = st.Ino
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		e.Op |= IsSymlink
	case fi.IsDir():
		e.Op |= IsDir
		// A folder created or moved in (inotify reports the new name of a
		// move as Create) needs watches for itself and its subfolders.
		if e.Op.Has(Create) {
			if err := w.addTree(e.Path); err != nil {
				e.Op |= MustRescan
			}
		}
	default:
		e.Op |= IsFile
	}
	return e
}

// CurrentEventID is 0: inotify has no event ids.
func CurrentEventID() uint64 { return 0 }
