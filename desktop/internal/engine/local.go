package engine

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
	"github.com/do-service/cloud/desktop/internal/watch"
)

// MarkDirty queues what a watcher burst says changed, for the next pass
// to rescan; RootChanged or MustRescan of the root means a full scan.
func (e *Engine) MarkDirty(d watch.Dirty) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if d.RootChanged {
		e.fullScan = true
	}
	for _, p := range d.Paths {
		if p == "" {
			e.fullScan = true
			continue
		}
		e.dirty[p] = true
	}
}

// RescanAll makes the next pass scan the whole folder.
func (e *Engine) RescanAll() {
	e.mu.Lock()
	e.fullScan = true
	e.mu.Unlock()
}

// refreshLocal brings the local table up to date with the disk: a full
// stat scan on the first pass, hourly, after a watcher overflow, and
// every FallbackRescan without a watcher; otherwise the dirty subtrees.
func (e *Engine) refreshLocal(ctx context.Context) error {
	e.mu.Lock()
	interval := e.cfg.RescanInterval
	if !e.watching {
		interval = e.cfg.FallbackRescan
	}
	full := !e.scanned || e.fullScan || time.Since(e.lastFull) >= interval
	dirty := e.dirty
	e.dirty = map[string]bool{}
	e.fullScan = false
	e.mu.Unlock()
	if full {
		res, err := localfs.Scan(e.cfg.Root, "")
		if err != nil {
			return err
		}
		if err := e.applyScan(ctx, "", res); err != nil {
			return err
		}
		e.mu.Lock()
		e.scanned, e.lastFull = true, time.Now()
		e.mu.Unlock()
		return nil
	}
	for p := range dirty {
		res, err := localfs.Scan(e.cfg.Root, p)
		if err != nil {
			return err
		}
		if err := e.applyScan(ctx, p, res); err != nil {
			return err
		}
	}
	return nil
}

// applyScan replaces what the table has at and below sub with a scan of
// it, keeping hashes of entries whose stamps didn't change.
func (e *Engine) applyScan(ctx context.Context, sub string, res localfs.Result) error {
	for _, s := range res.Skipped {
		e.log.Debug("sync: skipped", "path", s.RelPath, "reason", s.Reason, "err", s.Err)
	}
	return e.db.Update(ctx, func(tx *state.Tx) error {
		old := map[string]state.Local{}
		if err := tx.EachLocal(func(l state.Local) error {
			if under(l.RelPath, sub) {
				old[l.RelPath] = l
			}
			return nil
		}); err != nil {
			return err
		}
		if err := tx.DeleteLocal(sub); err != nil {
			return err
		}
		for _, en := range res.Entries {
			row := state.Local{RelPath: en.RelPath, Kind: state.Kind(en.Kind), Size: en.Size, MtimeNS: en.MtimeNS, FileID: en.FileID.String()}
			if o, ok := old[en.RelPath]; ok && o.SHA256Cached != "" && lstamp(&o).Same(en.Stamp) {
				row.SHA256Cached = o.SHA256Cached
			}
			if err := tx.PutLocal(row); err != nil {
				return err
			}
		}
		return nil
	})
}

// snapshot loads the three states and hashes the files the plan needs a
// hash of: stable ones that differ from base at their path. It also
// returns how many files base has (for the mass-delete guard).
func (e *Engine) snapshot(ctx context.Context) (*Snapshot, int, error) {
	s := &Snapshot{RootID: e.rootID, Now: time.Now(), Options: e.cfg.Options,
		BusyNodes: map[string]bool{}, BusyPaths: map[string]bool{}}
	synced := 0
	err := e.db.View(ctx, func(tx *state.Tx) error {
		if err := tx.EachRemote(func(r state.Remote) error { s.Remote = append(s.Remote, r); return nil }); err != nil {
			return err
		}
		if err := tx.EachBase(func(b state.Base) error {
			s.Base = append(s.Base, b)
			if b.Kind == state.KindFile {
				synced++
			}
			return nil
		}); err != nil {
			return err
		}
		if err := tx.EachLocal(func(l state.Local) error { s.Local = append(s.Local, l); return nil }); err != nil {
			return err
		}
		var err error
		if s.Rules, err = tx.Rules(); err != nil {
			return err
		}
		adopted, err := tx.Adopted()
		if err != nil {
			return err
		}
		s.Adopted = make(map[string]bool, len(adopted))
		for _, a := range adopted {
			s.Adopted[nfc(a)] = true
		}
		ops, err := tx.Ops()
		for _, o := range ops {
			busy(s, o.NodeID, o.Path)
			if o.Kind == string(OpRenameLocal) || o.Kind == string(OpConflict) || o.Kind == string(OpRescue) {
				if op, err := decodeOp(o); err == nil {
					s.BusyPaths[op.From] = true
				}
			}
		}
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	now := time.Now()
	e.mu.Lock()
	for k, b := range e.backoff {
		if now.Before(b.until) {
			if id, ok := strings.CutPrefix(k, "n:"); ok {
				s.BusyNodes[id] = true
			} else {
				s.BusyPaths[strings.TrimPrefix(k, "p:")] = true
			}
		}
	}
	e.mu.Unlock()
	return s, synced, e.hashLocal(ctx, s)
}

func busy(s *Snapshot, nodeID, rel string) {
	if nodeID != "" {
		s.BusyNodes[nodeID] = true
	}
	if rel != "" {
		s.BusyPaths[rel] = true
	}
}

// hashLocal fills SHA256Cached where the plan will compare or upload
// content, and saves it, so a hash is computed once per stamp.
func (e *Engine) hashLocal(ctx context.Context, s *Snapshot) error {
	baseAt := make(map[string]*state.Base, len(s.Base))
	for i := range s.Base {
		baseAt[s.Base[i].RelPath] = &s.Base[i]
	}
	var hashed []state.Local
	for i := range s.Local {
		l := &s.Local[i]
		if l.Kind != state.KindFile || l.SHA256Cached != "" {
			continue
		}
		if b := baseAt[l.RelPath]; b != nil && lstamp(l).Same(bstamp(b)) {
			continue
		}
		if s.Now.Sub(time.Unix(0, l.MtimeNS)) < s.Options.StableAfter {
			continue
		}
		h, err := localfs.Hash(e.cfg.Root, l.RelPath, localfs.Hashed{})
		if errors.Is(err, localfs.ErrChanged) || errors.Is(err, fs.ErrNotExist) {
			continue // being written, or gone: the next scan tells
		}
		if err != nil {
			e.log.Warn("sync: can't read a file", "path", l.RelPath, "err", err)
			continue
		}
		if !h.Stamp.Same(lstamp(l)) {
			// Changed since the scan: the stamp the hash belongs to wins.
			l.Size, l.MtimeNS, l.FileID = h.Size, h.MtimeNS, h.FileID.String()
		}
		l.SHA256Cached = h.SHA256
		hashed = append(hashed, *l)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if len(hashed) == 0 {
		return nil
	}
	return e.db.Update(ctx, func(tx *state.Tx) error {
		for _, l := range hashed {
			if err := tx.PutLocal(l); err != nil {
				return err
			}
		}
		return nil
	})
}

// cleanParts deletes the .~cloud-*.part files no download in the journal
// will resume: left by a download whose op was dropped, or by a crash.
func (e *Engine) cleanParts(ctx context.Context) error {
	keep := map[string]bool{}
	if err := e.db.View(ctx, func(tx *state.Tx) error {
		ops, err := tx.Ops()
		for _, o := range ops {
			if x, err := tx.Transfer(o.ID); err == nil && x.PartPath != "" {
				keep[filepath.Clean(x.PartPath)] = true
			}
		}
		return err
	}); err != nil {
		return err
	}
	return filepath.WalkDir(e.cfg.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable: the scan reports it
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		n := d.Name()
		if !d.IsDir() && strings.HasPrefix(n, ".~cloud-") && strings.HasSuffix(n, ".part") && !keep[filepath.Clean(p)] {
			if err := os.Remove(p); err == nil {
				e.log.Debug("sync: removed a stale partial download", "path", p)
			}
		}
		return nil
	})
}
