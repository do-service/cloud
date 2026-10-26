package watch

import (
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Dirty is what changed under a sync folder in a burst of events, ready
// for the engine: which subtrees to scan again.
type Dirty struct {
	// Paths are relative to the root, slash-separated, as the OS reported
	// them; each is to be rescanned with everything below it (a renamed
	// folder reports nothing for its contents), "" meaning the whole
	// folder. A path below another one in the list is left out.
	Paths []string
	// RootChanged: the sync folder itself was moved, deleted or replaced;
	// check the marker before anything else.
	RootChanged bool
	// HistoryDone: the replay from Options.Since ended within this burst.
	HistoryDone bool
	// LastID is the highest event id seen so far (0 where ids don't
	// exist). Replay ids aren't strictly increasing, so persist it only
	// once HistoryDone has been seen and these paths are handled.
	LastID uint64
}

// Debounce gathers the batches of a Watcher (in, under root — use
// Watcher.Root) into one Dirty per burst: sent once quiet has passed
// without events, or maxDelay after the first, so a file being written
// for minutes still shows up. The engine's 1–2 s is quiet 1 s, maxDelay
// 2 s. The channel closes after in does, with what was pending sent.
func Debounce(root string, in <-chan []Event, quiet, maxDelay time.Duration) <-chan Dirty {
	out := make(chan Dirty)
	go func() {
		defer close(out)
		d := newDirtySet(root)
		var timer *time.Timer
		var fire <-chan time.Time
		var first time.Time
		for {
			select {
			case batch, ok := <-in:
				if !ok {
					if d.pending() {
						out <- d.take()
					}
					return
				}
				for _, e := range batch {
					d.add(e)
				}
				if !d.pending() {
					continue
				}
				now := time.Now()
				if fire == nil {
					first = now
				}
				wait := min(quiet, first.Add(maxDelay).Sub(now))
				if timer == nil {
					timer = time.NewTimer(wait)
				} else {
					timer.Reset(wait)
				}
				fire = timer.C
			case <-fire:
				fire = nil
				out <- d.take()
			}
		}
	}()
	return out
}

type dirtySet struct {
	root        string
	paths       map[string]struct{}
	rootChanged bool
	historyDone bool
	lastID      uint64
}

func newDirtySet(root string) *dirtySet {
	return &dirtySet{root: filepath.Clean(root), paths: map[string]struct{}{}}
}

func (d *dirtySet) pending() bool {
	return len(d.paths) > 0 || d.rootChanged || d.historyDone
}

func (d *dirtySet) add(e Event) {
	if e.ID != SinceNow && e.ID > d.lastID {
		d.lastID = e.ID
	}
	if e.Op.Has(HistoryDone) {
		d.historyDone = true
		return
	}
	if e.Op.Has(RootChanged) {
		d.rootChanged = true
		return
	}
	if rel, ok := d.rel(e.Path); ok {
		d.paths[rel] = struct{}{}
	}
}

// rel maps an absolute event path to a slash path under root; false for
// paths outside it.
func (d *dirtySet) rel(p string) (string, bool) {
	p = filepath.Clean(p)
	if p == d.root {
		return "", true
	}
	prefix := d.root + string(filepath.Separator)
	if d.root == string(filepath.Separator) {
		prefix = d.root
	}
	if !strings.HasPrefix(p, prefix) {
		return "", false
	}
	return filepath.ToSlash(p[len(prefix):]), true
}

func (d *dirtySet) take() Dirty {
	out := Dirty{RootChanged: d.rootChanged, HistoryDone: d.historyDone, LastID: d.lastID}
	for p := range d.paths {
		if !d.covered(p) {
			out.Paths = append(out.Paths, p)
		}
	}
	sort.Strings(out.Paths)
	d.paths = map[string]struct{}{}
	d.rootChanged, d.historyDone = false, false
	return out
}

// covered reports whether an ancestor of p is dirty too, so scanning it
// covers p.
func (d *dirtySet) covered(p string) bool {
	if p == "" {
		return false
	}
	if _, ok := d.paths[""]; ok {
		return true
	}
	for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
		if _, ok := d.paths[dir]; ok {
			return true
		}
	}
	return false
}
