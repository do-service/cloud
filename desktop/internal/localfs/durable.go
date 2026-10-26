package localfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Durability of what Commit, Rename and Remove do, batched.
//
// The invariant the engine keeps: a change on disk is made durable before
// the op that made it is recorded as done in state.db, so after a power
// cut (or kill -9) nothing base claims is missing from the folder or has
// its old content. And a file's data is on the device before the rename
// that puts it in place, so a half-written file never replaces a good one.
//
// Each step costs differently. On macOS fsync(2) moves data to the drive
// but not out of its cache, and the drive may reorder writes; F_FULLFSYNC
// drains the device's whole cache (milliseconds, and serialized: ~5 ms a
// file, which made 1000 small downloads take 14 s); F_BARRIERFSYNC is an
// fsync plus a barrier: writes before it reach stable storage before
// writes after it, without waiting for them, and covers whatever was
// fsynced on the device before it, through any file (fcntl(2)). So a file
// is fsynced and a barrier issued before its rename — one barrier for all
// the files ready at the time (ordering is all they need) — and a batch
// of renames gets one F_FULLFSYNC per device before the batch's ops are
// recorded: F_FULLFSYNC guarantees whatever was fsynced on the device
// before it is persisted. On Linux fsync is durable already: the file is
// fsynced before its rename, the batch fsyncs each folder once. Windows
// flushes the file (FlushFileBuffers) and leaves the rename to NTFS's
// journal, as before.

// Syncer batches both flushes, group commit: an op that needs one asks,
// and whoever comes while no flush of that kind runs flushes everything
// asked so far for everyone. Ordering (the barrier a file needs before
// its rename) and durability (what the engine waits for before
// recording) are two such groups.
type Syncer struct {
	order, durable group
}

func NewSyncer() *Syncer {
	s := &Syncer{}
	s.order.init(barrierDirs)
	s.durable.init(flushDirs)
	return s
}

// Add queues folders for the next flush: changes made to them some other
// way, or found done after a crash and maybe not flushed.
func (s *Syncer) Add(dirs ...string) { s.durable.add(dirs...) }

// Wait returns once everything added before the call is durable, or the
// flush's error.
func (s *Syncer) Wait() error { return s.durable.wait() }

// group is one kind of flush shared by its waiters.
type group struct {
	mu       sync.Mutex
	cond     *sync.Cond
	dirs     map[string]bool
	gen      uint64 // the generation being collected
	done     uint64 // the last generation flushed
	flushing bool
	// errs of failed generations, kept while a waiter may still ask.
	errs    map[uint64]error
	waiters map[uint64]int
	flush   func(dirs []string) error
}

func (g *group) init(flush func([]string) error) {
	g.cond = sync.NewCond(&g.mu)
	g.dirs, g.gen, g.errs, g.waiters, g.flush = map[string]bool{}, 1, map[uint64]error{}, map[uint64]int{}, flush
}

func (g *group) add(dirs ...string) {
	g.mu.Lock()
	for _, d := range dirs {
		g.dirs[filepath.Clean(d)] = true
	}
	g.mu.Unlock()
}

func (g *group) wait() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	gen := g.gen
	g.waiters[gen]++
	defer func() {
		if g.waiters[gen]--; g.waiters[gen] == 0 {
			delete(g.waiters, gen)
			delete(g.errs, gen)
		}
	}()
	for g.done < gen {
		if g.flushing {
			g.cond.Wait()
			continue
		}
		// Lead this generation: what arrives meanwhile goes into the next.
		dirs := make([]string, 0, len(g.dirs))
		for d := range g.dirs {
			dirs = append(dirs, d)
		}
		g.dirs = map[string]bool{}
		lead := g.gen
		g.gen++
		g.flushing = true
		g.mu.Unlock()
		err := g.flush(dirs)
		g.mu.Lock()
		g.flushing = false
		g.done = lead
		if err != nil {
			g.errs[lead] = err
			// Not flushed: the next generation tries them again.
			for _, d := range dirs {
				g.dirs[d] = true
			}
		}
		g.cond.Broadcast()
	}
	return g.errs[gen]
}

// Rename is Rename with the folders' flush left to Wait.
func (s *Syncer) Rename(root, from, to string) error { return rename(root, from, to, s) }

// Remove is Remove with the folder's flush left to Wait.
func (s *Syncer) Remove(root, rel string, expect Stamp) error { return remove(root, rel, expect, s) }

// Mkdir is os.Mkdir of root/rel, its parent's flush left to Wait. A
// folder already there may be one made before a crash and not flushed:
// its parent is flushed too.
func (s *Syncer) Mkdir(root, rel string) error {
	if !validRel(rel) {
		return fmt.Errorf("localfs: bad relative path %q", rel)
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	err := os.Mkdir(abs, 0o755)
	if err == nil || errors.Is(err, fs.ErrExist) {
		s.durable.add(filepath.Dir(abs))
	}
	return err
}

// ordered puts what was fsynced in dir (a file in it) on stable storage
// before anything written after: now, or with s's group.
func ordered(s *Syncer, dir string) error {
	if s != nil {
		s.order.add(dir)
		return s.order.wait()
	}
	return barrierDirs([]string{dir})
}

// dirSynced makes dir's changed entries durable now, or with s's batch.
func dirSynced(s *Syncer, dirs ...string) error {
	if s != nil {
		s.durable.add(dirs...)
		return nil
	}
	for _, d := range dirs {
		if err := syncDir(d); err != nil {
			return err
		}
	}
	return nil
}
