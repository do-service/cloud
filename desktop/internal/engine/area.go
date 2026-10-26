package engine

import "github.com/do-service/cloud/desktop/internal/state"

// areaStatus is where a remote node stands in selective sync
// (docs/desktop-client.md, «Выборочная синхронизация»).
type areaStatus int8

const (
	// out: not synced; not on disk.
	out areaStatus = iota
	// container: a folder on the way to a selected node: on disk, but its
	// other children don't come.
	container
	// included: synced with everything below it, future children too.
	included
)

// area resolves the rules over the mirror: the nearest rule up a node's
// chain applies, the root's default is exclude (nothing selected on the
// first run), and so is «Доступно мне»'s: checking the root doesn't bring
// what others share, a new share isn't synced until checked. Rules on
// nodes the mirror doesn't have are ignored.
type area struct {
	rootID string
	remote map[string]*state.Remote
	rules  map[string]state.RuleMode
	eff    map[string]bool // id → effectively included
	cont   map[string]bool // id → container
}

func newArea(rootID string, remote map[string]*state.Remote, rules map[string]state.RuleMode) *area {
	a := &area{rootID: rootID, remote: remote, rules: rules, eff: map[string]bool{}, cont: map[string]bool{}}
	for id := range remote {
		if !a.includedRule(id) {
			continue
		}
		// Every folder above an included node that isn't included itself
		// is a container.
		for p := a.parentOf(id); p != ""; p = a.parentOf(p) {
			if a.includedRule(p) || a.cont[p] {
				break
			}
			a.cont[p] = true
		}
	}
	return a
}

func (a *area) parentOf(id string) string {
	if id == a.rootID {
		return ""
	}
	if r := a.remote[id]; r != nil {
		return r.ParentID
	}
	return ""
}

// includedRule is the nearest rule's verdict. A chain that doesn't reach
// the root (a node whose folder the mirror lacks) is out.
func (a *area) includedRule(id string) bool {
	if v, ok := a.eff[id]; ok {
		return v
	}
	var chain []string
	v := false
	for cur := id; ; {
		if c, ok := a.eff[cur]; ok {
			v = c
			break
		}
		chain = append(chain, cur)
		if m, ok := a.rules[cur]; ok {
			v = m == state.Include
			break
		}
		if cur == a.rootID || cur == SharedID {
			v = false
			break
		}
		r := a.remote[cur]
		if r == nil || r.ParentID == "" || len(chain) > 4096 {
			v = false
			break
		}
		cur = r.ParentID
	}
	for _, c := range chain {
		a.eff[c] = v
	}
	return v
}

func (a *area) status(id string) areaStatus {
	if a.remote[id] == nil && id != a.rootID {
		return out
	}
	if a.includedRule(id) {
		return included
	}
	if a.cont[id] {
		return container
	}
	return out
}

// in: on disk, either way.
func (a *area) in(id string) bool { return a.status(id) != out }
