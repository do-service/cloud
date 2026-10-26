package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// act performs a journaled op and returns how to record its result. Each
// first checks whether a previous run (cut off by a crash) did it.
func (e *Engine) act(ctx context.Context, id int64, o Op) (func(*state.Tx) error, error) {
	switch o.Kind {
	case OpUpload:
		return e.upload(ctx, id, o)
	case OpDownload:
		return e.download(ctx, id, o)
	case OpMkdirRemote:
		return e.mkdirRemote(ctx, o)
	case OpMoveRemote:
		return e.moveRemote(ctx, o)
	case OpTrashRemote:
		return e.trashRemote(ctx, o)
	case OpMkdirLocal:
		return e.mkdirLocal(o)
	case OpRenameLocal, OpConflict, OpRescue:
		return e.renameLocal(ctx, o)
	case OpRemoveLocal:
		return e.removeLocal(o)
	}
	return nil, fmt.Errorf("engine: unknown op %q", o.Kind)
}

func (e *Engine) abs(rel string) string { return filepath.Join(e.cfg.Root, filepath.FromSlash(rel)) }

// existsExact reports whether rel exists under exactly its name (up to
// Unicode normalization): on a case-insensitive disk a stat of "a.txt"
// finds "A.TXT", which is another node.
func (e *Engine) existsExact(rel string) (bool, error) {
	if _, err := os.Lstat(e.abs(rel)); err != nil {
		if notExist(err) {
			return false, nil
		}
		return false, err
	}
	ents, err := os.ReadDir(e.abs(dirOf(rel)))
	if err != nil {
		return false, err
	}
	want := nfc(nameOf(rel))
	for _, en := range ents {
		if nfc(en.Name()) == want {
			return true, nil
		}
	}
	return false, nil
}

// applyBaseOp records a base-only op.
func (e *Engine) applyBaseOp(tx *state.Tx, o Op) error {
	switch o.Kind {
	case OpSetBase:
		if err := putBase(tx, *o.Base); err != nil {
			return err
		}
		if o.Include {
			return tx.SetRule(o.NodeID, state.Include)
		}
		return nil
	case OpRebase:
		return moveBase(tx, o.NodeID, o.BasePath, o.Path)
	case OpForget:
		return tx.DeleteBase(o.NodeID)
	}
	return fmt.Errorf("engine: %q is not a base op", o.Kind)
}

// putBase writes a base row, evicting a stale row of another node at the
// same path (one whose own op failed this pass): that node is then new
// to the planner, which only ever downloads or links it again.
func putBase(tx *state.Tx, b state.Base) error {
	if old, err := tx.BaseByPath(b.RelPath); err == nil && old.NodeID != b.NodeID {
		if err := tx.DeleteBase(old.NodeID); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, state.ErrNotFound) {
		return err
	}
	return tx.PutBase(b)
}

// moveBase re-paths a node's base row and the rows below it. A row of
// another node where one of them lands is stale (its own op failed, or
// base followed a path the disk no longer has): it goes, and that node is
// new to the next plan, which only links, renames aside or fetches it.
func moveBase(tx *state.Tx, nodeID, from, to string) error {
	if from == to {
		return nil
	}
	moving, err := tx.BaseUnder(from)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, b := range moving {
		ids[b.NodeID] = true
	}
	for _, b := range moving {
		if old, err := tx.BaseByPath(to + b.RelPath[len(from):]); err == nil && !ids[old.NodeID] {
			if err := tx.DeleteBase(old.NodeID); err != nil {
				return err
			}
		} else if err != nil && !errors.Is(err, state.ErrNotFound) {
			return err
		}
	}
	return tx.MoveBase(from, to)
}

func (e *Engine) upload(ctx context.Context, id int64, o Op) (func(*state.Tx) error, error) {
	f, err := os.Open(e.abs(o.Path))
	if notExist(err) {
		return nil, fmt.Errorf("%w: %s is gone", errReplan, o.Path)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := localfs.Stat(e.cfg.Root, o.Path); err != nil || o.Expect == nil || !st.Same(*o.Expect) {
		return nil, fmt.Errorf("%w: %s changed since it was hashed", localfs.ErrChanged, o.Path)
	}
	var resume string
	_ = e.db.View(ctx, func(tx *state.Tx) error {
		if x, err := tx.Transfer(id); err == nil {
			resume = x.TusURL
		}
		return nil
	})
	u := api.Upload{Size: o.Size, MTime: time.Unix(0, o.MtimeNS), SHA256: o.SHA256, Body: f, URL: resume}
	if o.Size >= resumeFrom {
		// Worth resuming after a restart: the upload's URL and offset go
		// to the journal. A smaller file is cheaper to send again than to
		// journal (a commit is an fsync).
		u.Progress = func(url string, offset int64) error {
			return e.db.Update(context.WithoutCancel(ctx), func(tx *state.Tx) error {
				return tx.PutTransfer(state.Transfer{OpID: id, TusURL: url, Done: offset})
			})
		}
	}
	existing := o.Forget == "" && o.NodeID != ""
	if existing {
		u.NodeID, u.IfMatch = o.NodeID, o.IfMatch
	} else {
		u.ParentID, u.Name, u.OnConflict = o.ParentID, o.Name, api.ConflictFail
	}
	res, err := e.r.Upload(ctx, u)
	if existing && errors.Is(err, api.ErrPrecondition) {
		// Maybe this content is the node's already: an earlier attempt
		// whose answer was lost.
		if n, gerr := e.r.Get(ctx, o.NodeID); gerr == nil && n.SHA256 == o.SHA256 {
			res, err = api.UploadResult{NodeID: n.ID, ETag: n.ETag}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	// The server checked the content against SHA256, so what it has is
	// what was hashed. If the file changed meanwhile its stamp no longer
	// matches the base written here, and the next pass uploads again.
	return func(tx *state.Tx) error {
		if o.Forget != "" {
			if err := tx.DeleteBase(o.Forget); err != nil {
				return err
			}
		}
		if err := putBase(tx, state.Base{NodeID: res.NodeID, RelPath: o.Path, Kind: state.KindFile, SHA256: o.SHA256,
			ETag: res.ETag, FileID: o.Expect.FileID.String(), Size: o.Expect.Size, MtimeNS: o.Expect.MtimeNS}); err != nil {
			return err
		}
		r, err := tx.Remote(res.NodeID)
		switch {
		case err == nil:
			r.ETag, r.SHA256, r.Size, r.MtimeNS = res.ETag, o.SHA256, o.Size, o.MtimeNS
		case errors.Is(err, state.ErrNotFound):
			perms := state.PermRead | state.PermWrite
			if p, err := tx.Remote(o.ParentID); err == nil {
				perms = p.Perms
			}
			r = state.Remote{NodeID: res.NodeID, ParentID: o.ParentID, Name: o.Name, NameKey: localfs.NameKey(o.Name),
				Kind: state.KindFile, Size: o.Size, ETag: res.ETag, SHA256: o.SHA256, MtimeNS: o.MtimeNS, Perms: perms}
		default:
			return err
		}
		if err := tx.PutRemote(r); err != nil {
			return err
		}
		if err := tx.PutLocal(state.Local{RelPath: o.Path, Kind: state.KindFile, Size: o.Expect.Size, MtimeNS: o.Expect.MtimeNS,
			FileID: o.Expect.FileID.String(), SHA256Cached: o.SHA256}); err != nil {
			return err
		}
		if o.Include {
			if err := tx.SetRule(res.NodeID, state.Include); err != nil {
				return err
			}
		}
		return tx.AddActivity(state.Activity{Kind: "upload", RelPath: o.Path})
	}, nil
}

// resumeFrom: transfers from this size on are journaled to resume.
const resumeFrom = 4 << 20

func (e *Engine) download(ctx context.Context, id int64, o Op) (func(*state.Tx) error, error) {
	record := func(en localfs.Entry, sha, etag string) func(*state.Tx) error {
		return func(tx *state.Tx) error {
			if err := putBase(tx, state.Base{NodeID: o.NodeID, RelPath: o.Path, Kind: state.KindFile, SHA256: sha, ETag: etag,
				FileID: en.FileID.String(), Size: en.Size, MtimeNS: en.MtimeNS}); err != nil {
				return err
			}
			if err := tx.PutLocal(state.Local{RelPath: o.Path, Kind: state.KindFile, Size: en.Size, MtimeNS: en.MtimeNS,
				FileID: en.FileID.String(), SHA256Cached: sha}); err != nil {
				return err
			}
			if etag != o.ETag {
				// The content changed on the server while downloading: the
				// mirror learns the new one now, not a pass later.
				if r, err := tx.Remote(o.NodeID); err == nil {
					r.ETag, r.SHA256, r.Size = etag, sha, en.Size
					if err := tx.PutRemote(r); err != nil {
						return err
					}
				}
			}
			return tx.AddActivity(state.Activity{Kind: "download", RelPath: o.Path})
		}
	}
	// Done before a crash: the file is in place with the content.
	if there, _ := e.existsExact(o.Path); there {
		st, err := localfs.Stat(e.cfg.Root, o.Path)
		if err == nil && st.Kind == localfs.KindFile && (o.Expect == nil || !st.Same(*o.Expect)) {
			if h, err := localfs.Hash(e.cfg.Root, o.Path, localfs.Hashed{}); err == nil && h.SHA256 == o.SHA256 {
				e.sync.Add(e.abs(dirOf(o.Path)))
				return record(localfs.Entry{RelPath: o.Path, Stamp: h.Stamp}, o.SHA256, o.ETag), nil
			}
		}
	}
	part, err := localfs.PartPath(e.cfg.Root, o.Path, o.NodeID)
	if err != nil {
		return nil, err
	}
	if o.Size >= resumeFrom {
		// Kept across a restart; a small one's stray part goes then.
		if err := e.db.Update(ctx, func(tx *state.Tx) error {
			return tx.PutTransfer(state.Transfer{OpID: id, PartPath: part})
		}); err != nil {
			return nil, err
		}
	}
	res, err := e.r.DownloadFile(ctx, o.NodeID, part, api.DownloadOptions{ETag: o.ETag, SHA256: o.SHA256})
	releaseSlot(ctx)
	if notExist(err) {
		// The folder isn't there (yet): plan again.
		return nil, fmt.Errorf("%w: %v", errReplan, err)
	}
	if errors.Is(err, api.ErrNotFound) {
		os.Remove(part)
		return nil, fmt.Errorf("%w: gone on the server", errReplan)
	}
	if err != nil {
		return nil, err
	}
	var mtime time.Time
	if res.ETag == o.ETag && o.MtimeNS > 0 {
		mtime = time.Unix(0, o.MtimeNS)
	}
	en, err := localfs.Commit(e.cfg.Root, o.Path, part, localfs.CommitOptions{SHA256: res.SHA256, MTime: mtime, Expect: o.Expect, Sync: e.sync})
	if err != nil {
		// Edited or created locally meanwhile (ErrChanged): the plan
		// changes, and a part kept for it would linger in the folder.
		os.Remove(part)
		return nil, err
	}
	return record(en, res.SHA256, res.ETag), nil
}

func (e *Engine) mkdirRemote(ctx context.Context, o Op) (func(*state.Tx) error, error) {
	st, err := localfs.Stat(e.cfg.Root, o.Path)
	if err != nil || st.Kind != localfs.KindDir {
		return nil, fmt.Errorf("%w: %s is no longer a folder", errReplan, o.Path)
	}
	n, err := e.r.Mkdir(ctx, o.ParentID, o.Name)
	if err != nil {
		// A name conflict after a crash is likely this folder: the next
		// pass sees it in the mirror and links the two.
		return nil, err
	}
	return func(tx *state.Tx) error {
		if o.Forget != "" {
			if err := tx.DeleteBase(o.Forget); err != nil {
				return err
			}
		}
		r := toRemote(n)
		if r.ParentID == "" {
			r.ParentID = o.ParentID
		}
		if err := tx.PutRemote(r); err != nil {
			return err
		}
		if err := putBase(tx, state.Base{NodeID: n.ID, RelPath: o.Path, Kind: state.KindDir, ETag: n.ETag, FileID: st.FileID.String()}); err != nil {
			return err
		}
		if o.Include {
			return tx.SetRule(n.ID, state.Include)
		}
		return nil
	}, nil
}

func (e *Engine) moveRemote(ctx context.Context, o Op) (func(*state.Tx) error, error) {
	n, err := e.r.Move(ctx, o.NodeID, o.ParentID, o.Name, o.IfMatch)
	if err != nil {
		return nil, err
	}
	return func(tx *state.Tx) error {
		if err := moveBase(tx, o.NodeID, o.BasePath, o.Path); err != nil {
			return err
		}
		old, err := tx.Remote(o.NodeID)
		r := toRemote(n)
		if r.ParentID == "" {
			r.ParentID = o.ParentID
		}
		if err == nil && r.Kind == state.KindFile && r.SHA256 == "" {
			r.SHA256 = old.SHA256
		}
		if err := tx.PutRemote(r); err != nil {
			return err
		}
		return tx.AddActivity(state.Activity{Kind: "move", RelPath: o.Path, Detail: o.BasePath})
	}, nil
}

func (e *Engine) trashRemote(ctx context.Context, o Op) (func(*state.Tx) error, error) {
	ifMatch := o.IfMatch
	if o.Dir {
		// A folder's ETag in the mirror is stale (ancestors aren't
		// journaled): take the current one, then pull the changes made
		// up to it, and check the folder still holds nothing the client
		// hasn't seen. Anything added later changes the ETag, and the
		// trash fails.
		n, err := e.r.Get(ctx, o.NodeID)
		if errors.Is(err, api.ErrNotFound) {
			return e.trashed(o), nil
		}
		if err != nil {
			return nil, err
		}
		if err := e.pullRemote(ctx); err != nil {
			return nil, err
		}
		if ok, err := e.stillDeletable(ctx, o); err != nil || !ok {
			if err == nil {
				err = fmt.Errorf("%w: %s has something new on the server", errReplan, o.Path)
			}
			return nil, err
		}
		ifMatch = n.ETag
	}
	err := e.r.Trash(ctx, o.NodeID, ifMatch)
	if errors.Is(err, api.ErrNotFound) {
		// Trashed already (an earlier attempt, or someone else).
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return e.trashed(o), nil
}

func (e *Engine) trashed(o Op) func(*state.Tx) error {
	return func(tx *state.Tx) error {
		// The subtree's base rows by node, not by path: another folder may
		// sit at that path locally by now.
		var ids []string
		var walk func(id string) error
		walk = func(id string) error {
			ids = append(ids, id)
			kids, err := tx.RemoteChildren(id)
			for _, k := range kids {
				if err := walk(k.NodeID); err != nil {
					return err
				}
			}
			return err
		}
		if err := walk(o.NodeID); err != nil {
			return err
		}
		for _, id := range ids {
			if err := tx.DeleteBase(id); err != nil {
				return err
			}
		}
		if err := deleteRemoteTree(tx, o.NodeID); err != nil {
			return err
		}
		return tx.AddActivity(state.Activity{Kind: "trash", RelPath: o.Path})
	}
}

// stillDeletable re-checks on the fresh mirror what the plan saw: the
// folder is gone locally, and everything below it on the server is known
// and unchanged.
func (e *Engine) stillDeletable(ctx context.Context, o Op) (bool, error) {
	// The folder itself there again (by file id) came back after the
	// scan; anything else there is a new entry, uploaded after the trash.
	if there, err := e.existsExact(o.Path); err != nil {
		return false, err
	} else if there {
		st, err := localfs.Stat(e.cfg.Root, o.Path)
		if err != nil {
			return false, err
		}
		if st.Kind == localfs.KindDir && (o.Expect == nil || o.Expect.FileID.IsZero() || st.FileID == o.Expect.FileID) {
			return false, nil
		}
	}
	ok := true
	err := e.db.View(ctx, func(tx *state.Tx) error {
		var walk func(id string) error
		walk = func(id string) error {
			kids, err := tx.RemoteChildren(id)
			if err != nil {
				return err
			}
			for _, k := range kids {
				b, err := tx.Base(k.NodeID)
				if errors.Is(err, state.ErrNotFound) || err == nil && k.Kind == state.KindFile && remoteChanged(&k, &b) {
					ok = false
					return nil
				}
				if err != nil {
					return err
				}
				if k.Kind == state.KindDir {
					if err := walk(k.NodeID); err != nil || !ok {
						return err
					}
				}
			}
			return nil
		}
		return walk(o.NodeID)
	})
	return ok, err
}

func (e *Engine) mkdirLocal(o Op) (func(*state.Tx) error, error) {
	err := e.sync.Mkdir(e.cfg.Root, o.Path)
	if errors.Is(err, os.ErrExist) {
		// Made before a crash — or another entry with the name in
		// another case, which isn't this folder.
		if there, xerr := e.existsExact(o.Path); xerr != nil || !there {
			return nil, fmt.Errorf("%w: %s is taken", errReplan, o.Path)
		}
	} else if err != nil {
		if notExist(err) {
			return nil, fmt.Errorf("%w: %v", errReplan, err)
		}
		return nil, err
	}
	st, err := localfs.Stat(e.cfg.Root, o.Path)
	if err != nil {
		return nil, err
	}
	if st.Kind != localfs.KindDir {
		return nil, fmt.Errorf("%w: %s is a file", errReplan, o.Path)
	}
	return func(tx *state.Tx) error {
		etag := ""
		if r, err := tx.Remote(o.NodeID); err == nil {
			etag = r.ETag
		}
		if err := putBase(tx, state.Base{NodeID: o.NodeID, RelPath: o.Path, Kind: state.KindDir, ETag: etag, FileID: st.FileID.String()}); err != nil {
			return err
		}
		return tx.PutLocal(state.Local{RelPath: o.Path, Kind: state.KindDir, MtimeNS: st.MtimeNS, FileID: st.FileID.String()})
	}, nil
}

func (e *Engine) renameLocal(ctx context.Context, o Op) (func(*state.Tx) error, error) {
	from, ferr := e.existsExact(o.From)
	to, terr := e.existsExact(o.Path)
	if ferr != nil || terr != nil || from || !to {
		// Not done before a crash: do it.
		if err := e.sync.Rename(e.cfg.Root, o.From, o.Path); err != nil {
			if notExist(err) || errors.Is(err, localfs.ErrExists) {
				return nil, fmt.Errorf("%w: %v", errReplan, err)
			}
			return nil, err
		}
	} else {
		e.sync.Add(e.abs(dirOf(o.From)), e.abs(dirOf(o.Path)))
	}
	return func(tx *state.Tx) error {
		if err := tx.MoveLocal(o.From, o.Path); err != nil {
			return err
		}
		switch {
		case o.Kind == OpConflict:
			// The copy keeps the file id; base mustn't take it for the
			// node moved, so the node's name is free for the server's
			// version.
			b, err := tx.Base(o.NodeID)
			if err != nil {
				return err
			}
			b.FileID = ""
			if err := tx.PutBase(b); err != nil {
				return err
			}
			if _, err := tx.PutProblem(state.Problem{Kind: ProblemConflict, RelPath: o.Path, NodeID: o.NodeID,
				Detail: "edited here and on the server; this is the local version of " + o.From}); err != nil {
				return err
			}
			return tx.AddActivity(state.Activity{Kind: "conflict", RelPath: o.Path, Detail: o.From})
		case o.Kind == OpRescue:
			// The node is gone for the user; what was moved is new in
			// their own tree, and the next pass uploads it there.
			if o.NodeID != "" {
				if err := tx.DeleteBase(o.NodeID); err != nil {
					return err
				}
			}
			if _, err := tx.PutProblem(state.Problem{Kind: ProblemRescued, RelPath: o.Path, NodeID: o.NodeID,
				Detail: "can't be uploaded where it was; moved here from " + o.From}); err != nil {
				return err
			}
			return tx.AddActivity(state.Activity{Kind: "rescue", RelPath: o.Path, Detail: o.From})
		case o.KeepBase:
			if o.NodeID == "" {
				_, err := tx.PutProblem(state.Problem{Kind: ProblemConflict, RelPath: o.Path,
					Detail: "the name is taken on the server; renamed from " + o.From})
				return err
			}
			return nil
		default:
			return moveBase(tx, o.NodeID, o.BasePath, o.Path)
		}
	}, nil
}

func (e *Engine) removeLocal(o Op) (func(*state.Tx) error, error) {
	var done []RemoveItem
	var failure error
	for _, it := range o.Items {
		if err := e.sync.Remove(e.cfg.Root, it.Path, it.Expect); err != nil {
			failure = err
			break
		}
		done = append(done, it)
	}
	apply := func(tx *state.Tx) error {
		for _, it := range done {
			if err := tx.DeleteBase(it.NodeID); err != nil {
				return err
			}
			if err := tx.DeleteLocal(it.Path); err != nil {
				return err
			}
		}
		if len(done) > 0 {
			return tx.AddActivity(state.Activity{Kind: "remove", RelPath: o.Path})
		}
		return nil
	}
	if failure != nil {
		return apply, failure
	}
	return apply, nil
}
