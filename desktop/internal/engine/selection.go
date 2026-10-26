package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// Selective sync (docs/desktop-client.md, «Выборочная синхронизация»):
// the user checks folders and files in the cloud; checking one includes
// its whole subtree, future children too; unchecking it removes from disk
// what is synced (after uploading local edits), and the next passes do
// the work.

// freeSpaceOf is freeSpace, replaced in tests.
var freeSpaceOf = freeSpace

var (
	ErrNotInCloud = errors.New("engine: no such file or folder in the cloud")
	// ErrNoSpace: what checking would download doesn't fit on the disk.
	ErrNoSpace = errors.New("engine: not enough free disk space")
)

// Prepare lists the server's tree if the mirror is empty, so paths can be
// resolved before the first sync. Once it has succeeded, Select,
// Unselect, Selection, Marks and Usage may be called while Run runs (the
// app's window does): they read the mirror from the database, write
// rules in transactions of their own, and see the own root through
// pullMu; only Prepare's first listing would race with a pass.
func (e *Engine) Prepare(ctx context.Context) error {
	if e.ownRoot() != "" {
		return nil
	}
	return e.pullRemote(ctx)
}

// ownRoot is rootID for the methods that may run alongside a pass: a
// resync after 410 sets it again, under pullMu.
func (e *Engine) ownRoot() string {
	e.pullMu.Lock()
	defer e.pullMu.Unlock()
	return e.rootID
}

// Wake makes a running Run pass now rather than after its long poll:
// it doesn't watch the rules, so Select and Unselect from another
// goroutine call this.
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Selected is a rule as the user sees it.
type Selected struct {
	// Path in the cloud, "/"-separated, "/" for the root.
	Path string
	Mode state.RuleMode
	Kind state.Kind
	// Missing: the rule's node isn't in the cloud any more.
	Missing bool
}

type tree struct {
	rootID string
	byID   map[string]*state.Remote
	kids   map[string]map[string]*state.Remote
}

func (e *Engine) loadTree(ctx context.Context) (*tree, map[string]state.RuleMode, error) {
	t := &tree{rootID: e.ownRoot(), byID: map[string]*state.Remote{}, kids: map[string]map[string]*state.Remote{}}
	var rules map[string]state.RuleMode
	err := e.db.View(ctx, func(tx *state.Tx) error {
		if err := tx.EachRemote(func(r state.Remote) error {
			rr := r
			t.byID[r.NodeID] = &rr
			return nil
		}); err != nil {
			return err
		}
		var err error
		rules, err = tx.Rules()
		return err
	})
	for _, r := range t.byID {
		if r.NodeID == t.rootID {
			continue
		}
		m := t.kids[r.ParentID]
		if m == nil {
			m = map[string]*state.Remote{}
			t.kids[r.ParentID] = m
		}
		m[keyOf(r)] = r
	}
	return t, rules, err
}

// resolve finds a cloud path, comparing names as the server does.
func (t *tree) resolve(p string) (*state.Remote, error) {
	cur := t.byID[t.rootID]
	if cur == nil {
		return nil, ErrNotInCloud
	}
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part == "" {
			continue
		}
		next := t.kids[cur.NodeID][localfs.NameKey(part)]
		if next == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotInCloud, p)
		}
		cur = next
	}
	return cur, nil
}

// node finds a node of the mirror by id.
func (t *tree) node(id string) (*state.Remote, error) {
	if n := t.byID[id]; n != nil {
		return n, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotInCloud, id)
}

func (t *tree) path(id string) (string, bool) {
	var parts []string
	for cur := id; cur != t.rootID; {
		r := t.byID[cur]
		if r == nil || len(parts) > 4096 {
			return "", false
		}
		parts = append(parts, r.Name)
		cur = r.ParentID
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return "/" + strings.Join(parts, "/"), true
}

// Select checks the cloud path p. Before that it checks that what it
// brings fits on the disk (where the OS tells). It returns how many bytes
// will be downloaded.
func (e *Engine) Select(ctx context.Context, p string) (int64, error) {
	return e.selectBy(ctx, func(t *tree) (*state.Remote, error) { return t.resolve(p) }, true)
}

// SelectNode is Select by node id, as the window knows the cloud's items:
// a mirror id, SharedID for «Доступно мне» included.
func (e *Engine) SelectNode(ctx context.Context, id string) (int64, error) {
	return e.selectBy(ctx, func(t *tree) (*state.Remote, error) { return t.node(id) }, true)
}

// Unselect unchecks the cloud path p: its own include rule goes when the
// folder above isn't included (it then inherits exclude), else it gets
// an exclude rule.
func (e *Engine) Unselect(ctx context.Context, p string) error {
	_, err := e.selectBy(ctx, func(t *tree) (*state.Remote, error) { return t.resolve(p) }, false)
	return err
}

// UnselectNode is Unselect by node id.
func (e *Engine) UnselectNode(ctx context.Context, id string) error {
	_, err := e.selectBy(ctx, func(t *tree) (*state.Remote, error) { return t.node(id) }, false)
	return err
}

func (e *Engine) selectBy(ctx context.Context, find func(*tree) (*state.Remote, error), include bool) (int64, error) {
	if err := e.Prepare(ctx); err != nil {
		return 0, err
	}
	t, rules, err := e.loadTree(ctx)
	if err != nil {
		return 0, err
	}
	n, err := find(t)
	if err != nil {
		return 0, err
	}
	if !include {
		parentIncluded := false
		if n.NodeID != t.rootID {
			parentIncluded = newArea(t.rootID, t.byID, rules).status(n.ParentID) == included
		}
		return 0, e.db.Update(ctx, func(tx *state.Tx) error {
			if parentIncluded {
				return tx.SetRule(n.NodeID, state.Exclude)
			}
			return tx.DeleteRule(n.NodeID)
		})
	}
	known := map[string]bool{}
	if err := e.db.View(ctx, func(tx *state.Tx) error {
		return tx.EachBase(func(b state.Base) error { known[b.NodeID] = true; return nil })
	}); err != nil {
		return 0, err
	}
	before := newArea(t.rootID, t.byID, rules)
	after := map[string]state.RuleMode{}
	for k, v := range rules {
		after[k] = v
	}
	after[n.NodeID] = state.Include
	next := newArea(t.rootID, t.byID, after)
	var need int64
	for id, r := range t.byID {
		if r.Kind == state.KindFile && !known[id] && next.status(id) == included && before.status(id) != included {
			need += r.Size
		}
	}
	if free, ok := freeSpaceOf(e.cfg.Root); ok && need > free {
		return need, fmt.Errorf("%w: %d bytes to download, %d free", ErrNoSpace, need, free)
	}
	err = e.db.Update(ctx, func(tx *state.Tx) error { return tx.SetRule(n.NodeID, state.Include) })
	return need, err
}

// Selection lists the rules, by path.
func (e *Engine) Selection(ctx context.Context) ([]Selected, error) {
	t, rules, err := e.loadTree(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Selected, 0, len(rules))
	for id, mode := range rules {
		s := Selected{Mode: mode}
		if p, ok := t.path(id); ok {
			s.Path = p
			if r := t.byID[id]; r != nil {
				s.Kind = r.Kind
			}
		} else {
			s.Path, s.Missing = id, true
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Adoptable lists the items at the top of the cloud that the sync folder
// held under the same name when it was connected (state.Tx.Adopted) and
// are still there, but aren't checked: connecting leaves them alone
// (planner.leftAlone), and the window offers to check them at once
// («Отметить папки, которые уже есть на диске»). Checked, each pairs with
// what is on disk. «Доступно мне» is left out: checking it brings every
// share.
func (e *Engine) Adoptable(ctx context.Context) ([]string, error) {
	if e.ownRoot() == "" {
		return nil, ErrNotReady
	}
	t, rules, err := e.loadTree(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	if err := e.db.View(ctx, func(tx *state.Tx) error {
		var err error
		names, err = tx.Adopted()
		return err
	}); err != nil {
		return nil, err
	}
	a := newArea(t.rootID, t.byID, rules)
	var out []string
	for _, n := range names {
		r := t.kids[t.rootID][localfs.NameKey(n)]
		if r == nil || r.NodeID == SharedID || a.in(r.NodeID) {
			continue
		}
		st, err := localfs.Stat(e.cfg.Root, n)
		if err != nil || state.Kind(st.Kind) != r.Kind {
			continue
		}
		out = append(out, r.NodeID)
	}
	return out, nil
}
