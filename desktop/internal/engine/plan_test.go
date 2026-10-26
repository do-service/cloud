package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// world builds a snapshot: a synced tree, then changes to one side or
// both, then the plan. Times are far in the past, so every file is
// stable unless a test says otherwise.

var (
	t0  = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	old = t0.Add(-time.Hour).UnixNano()
)

const rw = state.PermRead | state.PermWrite

type world struct {
	s     Snapshot
	fid   int
	paths map[string]string // node id → local path at sync
}

func newWorld() *world {
	w := &world{s: Snapshot{RootID: "R", Rules: map[string]state.RuleMode{"R": state.Include}, Now: t0,
		Options: Options{DeviceName: "Mac", StableAfter: 3 * time.Second}}, paths: map[string]string{}}
	w.s.Remote = append(w.s.Remote, state.Remote{NodeID: "R", Kind: state.KindDir, ETag: "r", Perms: rw})
	return w
}

func (w *world) nextFID() string { w.fid++; return fmt.Sprintf("1:%x", w.fid) }

// synced adds a node both sides and base agree on; parent is a node id,
// content the file's (sha "h-"+content).
func (w *world) synced(id, parent, name string, kind state.Kind, content string) {
	rel := name
	if parent != "R" {
		rel = w.paths[parent] + "/" + name
	}
	w.paths[id] = rel
	sha, size := "", int64(0)
	if kind == state.KindFile {
		sha, size = "h-"+content, int64(len(content))
	}
	fid := w.nextFID()
	w.s.Remote = append(w.s.Remote, state.Remote{NodeID: id, ParentID: parent, Name: name, NameKey: localfs.NameKey(name),
		Kind: kind, Size: size, ETag: "e-" + id + "-" + content, SHA256: sha, MtimeNS: old, Perms: rw})
	w.s.Base = append(w.s.Base, state.Base{NodeID: id, RelPath: rel, Kind: kind, SHA256: sha, ETag: "e-" + id + "-" + content,
		FileID: fid, Size: size, MtimeNS: old})
	w.s.Local = append(w.s.Local, state.Local{RelPath: rel, Kind: kind, Size: size, MtimeNS: old, FileID: fid, SHA256Cached: sha})
}

func (w *world) remote(id string) *state.Remote {
	for i := range w.s.Remote {
		if w.s.Remote[i].NodeID == id {
			return &w.s.Remote[i]
		}
	}
	panic("no remote " + id)
}

func (w *world) local(rel string) *state.Local {
	for i := range w.s.Local {
		if w.s.Local[i].RelPath == rel {
			return &w.s.Local[i]
		}
	}
	panic("no local " + rel)
}

// Remote changes.
func (w *world) rEdit(id, content string) {
	r := w.remote(id)
	r.ETag, r.SHA256, r.Size = "e-"+id+"-"+content, "h-"+content, int64(len(content))
}
func (w *world) rMove(id, parent, name string) {
	r := w.remote(id)
	r.ParentID, r.Name, r.NameKey = parent, name, localfs.NameKey(name)
}
func (w *world) rDelete(ids ...string) {
	w.s.Remote = slices.DeleteFunc(w.s.Remote, func(r state.Remote) bool { return slices.Contains(ids, r.NodeID) })
}
func (w *world) rNew(id, parent, name string, kind state.Kind, content string) {
	sha := ""
	if kind == state.KindFile {
		sha = "h-" + content
	}
	w.s.Remote = append(w.s.Remote, state.Remote{NodeID: id, ParentID: parent, Name: name, NameKey: localfs.NameKey(name),
		Kind: kind, Size: int64(len(content)), ETag: "e-" + id, SHA256: sha, MtimeNS: old, Perms: rw})
}

// Local changes.
func (w *world) lEdit(rel, content string) {
	l := w.local(rel)
	l.SHA256Cached, l.Size, l.MtimeNS = "h-"+content, int64(len(content)), old+int64(time.Second)
}
func (w *world) lMove(from, to string) {
	for i := range w.s.Local {
		if p := w.s.Local[i].RelPath; under(p, from) {
			w.s.Local[i].RelPath = to + p[len(from):]
		}
	}
}
func (w *world) lDelete(rel string) {
	w.s.Local = slices.DeleteFunc(w.s.Local, func(l state.Local) bool { return under(l.RelPath, rel) })
}
func (w *world) lNew(rel string, kind state.Kind, content string) {
	sha := ""
	if kind == state.KindFile {
		sha = "h-" + content
	}
	w.s.Local = append(w.s.Local, state.Local{RelPath: rel, Kind: kind, Size: int64(len(content)), MtimeNS: old,
		FileID: w.nextFID(), SHA256Cached: sha})
}

func (w *world) plan() Plan { return MakePlan(&w.s) }

// ops renders a plan as "kind path" lines, with the details a cell
// depends on.
func ops(p Plan) string {
	var out []string
	for _, o := range p.Ops {
		s := string(o.Kind) + " " + o.Path
		switch o.Kind {
		case OpRenameLocal, OpConflict, OpRescue:
			s = string(o.Kind) + " " + o.From + " -> " + o.Path
			if o.KeepBase {
				s += " keepbase"
			}
		case OpUpload:
			if o.NodeID != "" && o.Forget == "" {
				s += " node=" + o.NodeID + " if=" + o.IfMatch
			} else {
				s += " new in " + o.ParentID
			}
		case OpMoveRemote:
			s += " to " + o.ParentID + "/" + o.Name
		case OpTrashRemote:
			if o.IfMatch != "" {
				s += " if=" + o.IfMatch
			}
		case OpSetBase:
			s += " etag=" + o.Base.ETag
		}
		out = append(out, s)
	}
	return strings.Join(out, "\n")
}

func expectOps(t *testing.T, p Plan, want ...string) {
	t.Helper()
	if got := ops(p); got != strings.Join(want, "\n") {
		t.Errorf("plan:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

func problems(p Plan) string {
	var out []string
	for _, pr := range p.Problems {
		out = append(out, pr.Kind+" "+pr.RelPath)
	}
	return strings.Join(out, "\n")
}

// A file a.txt in folder D: the cells of the table in docs/desktop-client.md
// (local \ remote: unchanged, changed, deleted, moved).
func fileWorld() *world {
	w := newWorld()
	w.synced("D", "R", "D", state.KindDir, "")
	w.synced("E", "R", "E", state.KindDir, "")
	w.synced("F", "D", "a.txt", state.KindFile, "v1")
	return w
}

func TestCells(t *testing.T) {
	cases := []struct {
		name   string
		change func(w *world)
		want   []string
	}{
		{"unchanged/unchanged", func(w *world) {}, nil},
		{"edited/unchanged → upload with If-Match", func(w *world) { w.lEdit("D/a.txt", "v2") },
			[]string{"upload D/a.txt node=F if=e-F-v1"}},
		{"edited/edited same content → base", func(w *world) { w.lEdit("D/a.txt", "v2"); w.rEdit("F", "v2") },
			[]string{"set_base D/a.txt etag=e-F-v2"}},
		{"edited/edited → conflict copy", func(w *world) { w.lEdit("D/a.txt", "v2"); w.rEdit("F", "v3") },
			[]string{"conflict D/a.txt -> D/a (конфликт, Mac, 2026-10-25 12-00).txt"}},
		{"edited/deleted → upload as new", func(w *world) { w.lEdit("D/a.txt", "v2"); w.rDelete("F") },
			[]string{"upload D/a.txt new in D"}},
		{"edited/moved → move locally first", func(w *world) { w.lEdit("D/a.txt", "v2"); w.rMove("F", "E", "b.txt") },
			[]string{"rename_local D/a.txt -> E/b.txt"}},
		{"deleted/unchanged → trash with If-Match", func(w *world) { w.lDelete("D/a.txt") },
			[]string{"trash_remote D/a.txt if=e-F-v1"}},
		{"deleted/edited → download again", func(w *world) { w.lDelete("D/a.txt"); w.rEdit("F", "v2") },
			[]string{"download D/a.txt"}},
		{"deleted/deleted → forget", func(w *world) { w.lDelete("D/a.txt"); w.rDelete("F") },
			[]string{"forget D/a.txt"}},
		{"deleted/moved → trash with If-Match", func(w *world) { w.lDelete("D/a.txt"); w.rMove("F", "E", "b.txt") },
			[]string{"trash_remote D/a.txt if=e-F-v1"}},
		{"moved/unchanged → PATCH", func(w *world) { w.lMove("D/a.txt", "E/b.txt") },
			[]string{"move_remote E/b.txt to E/b.txt"}},
		{"moved/edited → PATCH (download follows)", func(w *world) { w.lMove("D/a.txt", "E/b.txt"); w.rEdit("F", "v2") },
			[]string{"move_remote E/b.txt to E/b.txt"}},
		{"moved/deleted → upload as new", func(w *world) { w.lMove("D/a.txt", "E/b.txt"); w.rDelete("F") },
			[]string{"upload E/b.txt new in E"}},
		{"moved/moved → server's position", func(w *world) { w.lMove("D/a.txt", "E/b.txt"); w.rMove("F", "R", "c.txt") },
			[]string{"rename_local E/b.txt -> c.txt"}},
		{"moved/moved to the same place → base", func(w *world) { w.lMove("D/a.txt", "E/b.txt"); w.rMove("F", "E", "b.txt") },
			[]string{"rebase E/b.txt"}},
		{"unchanged/edited → download", func(w *world) { w.rEdit("F", "v2") },
			[]string{"download D/a.txt"}},
		{"unchanged/deleted → remove locally", func(w *world) { w.rDelete("F") },
			[]string{"remove_local D/a.txt"}},
		{"unchanged/moved → rename locally", func(w *world) { w.rMove("F", "E", "b.txt") },
			[]string{"rename_local D/a.txt -> E/b.txt"}},
		{"touched (same content) → base only", func(w *world) {
			l := w.local("D/a.txt")
			l.MtimeNS += 5
		}, []string{"set_base D/a.txt etag=e-F-v1"}},
		{"replaced by an editor (new file id, same name) → upload", func(w *world) {
			l := w.local("D/a.txt")
			l.FileID, l.SHA256Cached, l.MtimeNS = "9:9", "h-v2", old+1
		}, []string{"upload D/a.txt node=F if=e-F-v1"}},
		{"new ETag, same sha256 (resync) → base only", func(w *world) { w.remote("F").ETag = "e-new" },
			[]string{"set_base D/a.txt etag=e-new"}},
		{"case-only rename on the server", func(w *world) { w.rMove("F", "D", "A.txt") },
			[]string{"rename_local D/a.txt -> D/A.txt"}},
		{"case-only rename locally", func(w *world) { w.lMove("D/a.txt", "D/A.txt") },
			[]string{"move_remote D/A.txt to D/A.txt"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := fileWorld()
			c.change(w)
			p := w.plan()
			expectOps(t, p, c.want...)
			if len(p.Problems) > 0 {
				t.Errorf("problems: %s", problems(p))
			}
		})
	}
}

// After a conflict copy is made, the next pass downloads the server's
// version under the name and uploads the copy as a new file.
func TestConflictFollowUp(t *testing.T) {
	w := fileWorld()
	w.rEdit("F", "v3")
	// The state after OpConflict: the edit moved aside, base without a
	// file id.
	w.lMove("D/a.txt", "D/a (конфликт, Mac, 2026-10-25 12-00).txt")
	w.lEdit("D/a (конфликт, Mac, 2026-10-25 12-00).txt", "v2")
	for i := range w.s.Base {
		if w.s.Base[i].NodeID == "F" {
			w.s.Base[i].FileID = ""
		}
	}
	expectOps(t, w.plan(),
		"upload D/a (конфликт, Mac, 2026-10-25 12-00).txt new in D",
		"download D/a.txt")
}

func TestUnstableFileWaits(t *testing.T) {
	w := fileWorld()
	w.lEdit("D/a.txt", "v2")
	w.local("D/a.txt").MtimeNS = t0.Add(-time.Second).UnixNano()
	w.rEdit("F", "v3")
	p := w.plan()
	expectOps(t, p)
	if p.Deferred == 0 || p.Unstable == 0 {
		t.Errorf("deferred %d, unstable %d", p.Deferred, p.Unstable)
	}
	w = newWorld()
	w.lNew("new.txt", state.KindFile, "x")
	w.local("new.txt").MtimeNS = t0.UnixNano()
	if p := w.plan(); len(p.Ops) != 0 || p.Unstable != 1 {
		t.Errorf("a file being written: %s, unstable %d", ops(p), p.Unstable)
	}
}

func TestNewBothSides(t *testing.T) {
	t.Run("new local file and folder", func(t *testing.T) {
		w := newWorld()
		w.lNew("dir", state.KindDir, "")
		w.lNew("dir/x.txt", state.KindFile, "x")
		w.lNew("a.txt", state.KindFile, "a")
		p := w.plan()
		// x.txt waits for its folder to exist on the server.
		expectOps(t, p, "mkdir_remote dir", "upload a.txt new in R")
		if p.Deferred != 1 {
			t.Errorf("deferred %d", p.Deferred)
		}
	})
	t.Run("new remote file and folder, in one pass", func(t *testing.T) {
		w := newWorld()
		w.rNew("D", "R", "dir", state.KindDir, "")
		w.rNew("X", "D", "x.txt", state.KindFile, "x")
		w.rNew("A", "R", "a.txt", state.KindFile, "a")
		expectOps(t, w.plan(), "mkdir_local dir", "download a.txt", "download dir/x.txt")
	})
	t.Run("same name, same content → link", func(t *testing.T) {
		w := newWorld()
		w.rNew("A", "R", "a.txt", state.KindFile, "same")
		w.lNew("A.TXT", state.KindFile, "same")
		expectOps(t, w.plan(), "set_base A.TXT etag=e-A")
	})
	t.Run("same name, other content → local aside, both kept", func(t *testing.T) {
		w := newWorld()
		w.rNew("A", "R", "a.txt", state.KindFile, "server")
		w.lNew("a.txt", state.KindFile, "local")
		expectOps(t, w.plan(), "rename_local a.txt -> a (конфликт, Mac, 2026-10-25 12-00).txt keepbase")
	})
	t.Run("two folders → link", func(t *testing.T) {
		w := newWorld()
		w.rNew("D", "R", "Docs", state.KindDir, "")
		w.lNew("docs", state.KindDir, "")
		expectOps(t, w.plan(), "set_base docs etag=e-D")
	})
	t.Run("file against folder → aside", func(t *testing.T) {
		w := newWorld()
		w.rNew("D", "R", "x", state.KindDir, "")
		w.lNew("x", state.KindFile, "f")
		expectOps(t, w.plan(), "rename_local x -> x (конфликт, Mac, 2026-10-25 12-00) keepbase")
	})
}

func TestFolderDeletes(t *testing.T) {
	tree := func() *world {
		w := newWorld()
		w.synced("D", "R", "D", state.KindDir, "")
		w.synced("S", "D", "sub", state.KindDir, "")
		w.synced("A", "D", "a.txt", state.KindFile, "a")
		w.synced("B", "S", "b.txt", state.KindFile, "b")
		return w
	}
	t.Run("deleted locally, nothing new on the server → one trash", func(t *testing.T) {
		w := tree()
		w.lDelete("D")
		p := w.plan()
		expectOps(t, p, "trash_remote D")
		if p.Deletes() != 2 {
			t.Errorf("deletes %d", p.Deletes())
		}
	})
	t.Run("deleted locally, a new file on the server → known files trashed, folder comes back", func(t *testing.T) {
		w := tree()
		w.lDelete("D")
		w.rNew("N", "S", "new.txt", state.KindFile, "n")
		p := w.plan()
		expectOps(t, p, "trash_remote D/sub/b.txt if=e-B-b", "trash_remote D/a.txt if=e-A-a")
		// Next pass: the known files are gone; the folders come back with
		// the new file.
		w.rDelete("A", "B")
		w.s.Base = slices.DeleteFunc(w.s.Base, func(b state.Base) bool { return b.NodeID == "A" || b.NodeID == "B" })
		expectOps(t, w.plan(), "mkdir_local D", "mkdir_local D/sub", "download D/sub/new.txt")
	})
	t.Run("deleted locally, a file edited on the server → it comes back", func(t *testing.T) {
		w := tree()
		w.lDelete("D")
		w.rEdit("B", "b2")
		expectOps(t, w.plan(), "trash_remote D/a.txt if=e-A-a")
	})
	t.Run("deleted on the server, unchanged locally → one remove", func(t *testing.T) {
		w := tree()
		w.rDelete("D", "S", "A", "B")
		p := w.plan()
		expectOps(t, p, "remove_local D")
		if p.Deletes() != 2 || len(p.Ops[0].Items) != 4 || p.Ops[0].Items[0].Path != "D/sub/b.txt" {
			t.Errorf("deletes %d, items %+v", p.Deletes(), p.Ops[0].Items)
		}
	})
	t.Run("deleted on the server, edited locally → folder re-created, unchanged removed", func(t *testing.T) {
		w := tree()
		w.rDelete("D", "S", "A", "B")
		w.lEdit("D/sub/b.txt", "b2")
		expectOps(t, w.plan(), "mkdir_remote D", "remove_local D/a.txt")
	})
	t.Run("deleted on the server, a new local file → folder re-created", func(t *testing.T) {
		w := tree()
		w.rDelete("D", "S", "A", "B")
		w.lNew("D/new.txt", state.KindFile, "n")
		expectOps(t, w.plan(), "mkdir_remote D", "remove_local D/a.txt", "remove_local D/sub")
	})
	t.Run("folder renamed on the server: children wait", func(t *testing.T) {
		w := tree()
		w.rMove("D", "R", "D2")
		w.rEdit("A", "a2")
		p := w.plan()
		expectOps(t, p, "rename_local D -> D2")
		if p.Deferred == 0 {
			t.Error("children not deferred")
		}
	})
	t.Run("folder renamed locally: one PATCH", func(t *testing.T) {
		w := tree()
		w.lMove("D", "D2")
		expectOps(t, w.plan(), "move_remote D2 to R/D2")
	})
}

func TestSelectiveSync(t *testing.T) {
	tree := func() *world {
		w := newWorld()
		w.s.Rules = map[string]state.RuleMode{}
		w.rNew("P", "R", "Photos", state.KindDir, "")
		w.rNew("Y", "P", "2026", state.KindDir, "")
		w.rNew("I", "Y", "img.jpg", state.KindFile, "i")
		w.rNew("O", "P", "old.jpg", state.KindFile, "o")
		w.rNew("T", "R", "top.txt", state.KindFile, "t")
		return w
	}
	t.Run("first run: nothing selected", func(t *testing.T) {
		expectOps(t, tree().plan())
	})
	t.Run("a selected folder brings its path as containers", func(t *testing.T) {
		w := tree()
		w.s.Rules["Y"] = state.Include
		expectOps(t, w.plan(), "mkdir_local Photos", "mkdir_local Photos/2026", "download Photos/2026/img.jpg")
	})
	t.Run("excluded below included", func(t *testing.T) {
		w := tree()
		w.s.Rules["P"] = state.Include
		w.s.Rules["Y"] = state.Exclude
		expectOps(t, w.plan(), "mkdir_local Photos", "download Photos/old.jpg")
	})
	t.Run("a new local file in a container gets included", func(t *testing.T) {
		w := tree()
		w.s.Rules["Y"] = state.Include
		w.synced2("P", "Photos")
		w.lNew("Photos/mine.txt", state.KindFile, "m")
		w.lNew("root.txt", state.KindFile, "r")
		p := w.plan()
		var inc []string
		for _, o := range p.Ops {
			if o.Kind == OpUpload && o.Include {
				inc = append(inc, o.Path)
			}
		}
		if strings.Join(inc, ",") != "root.txt,Photos/mine.txt" {
			t.Errorf("included uploads %v in\n%s", inc, ops(p))
		}
	})
	t.Run("unchecking: unchanged removed, an edit uploaded first", func(t *testing.T) {
		w := newWorld()
		w.s.Rules = map[string]state.RuleMode{"D": state.Include}
		w.synced("D", "R", "D", state.KindDir, "")
		w.synced("A", "D", "a.txt", state.KindFile, "a")
		w.synced("B", "D", "b.txt", state.KindFile, "b")
		w.lEdit("D/b.txt", "b2")
		w.lNew("D/n.txt", state.KindFile, "n")
		w.s.Rules["D"] = state.Exclude
		p := w.plan()
		expectOps(t, p, "remove_local D/a.txt", "upload D/b.txt node=B if=e-B-b", "upload D/n.txt new in D")
		for _, o := range p.Ops {
			if o.Include || o.Deletes != 0 {
				t.Errorf("%s: include %v, deletes %d", o.Kind, o.Include, o.Deletes)
			}
		}
	})
	t.Run("unchecking a fully synced folder removes it whole", func(t *testing.T) {
		w := newWorld()
		w.synced("D", "R", "D", state.KindDir, "")
		w.synced("A", "D", "a.txt", state.KindFile, "a")
		w.s.Rules = map[string]state.RuleMode{"D": state.Exclude}
		p := w.plan()
		expectOps(t, p, "remove_local D")
		if p.Deletes() != 0 {
			t.Errorf("unchecking counted as %d deletes", p.Deletes())
		}
	})
	t.Run("deleted locally in an unchecked folder → forgotten, not trashed", func(t *testing.T) {
		w := newWorld()
		w.synced("D", "R", "D", state.KindDir, "")
		w.synced("A", "D", "a.txt", state.KindFile, "a")
		w.s.Rules = map[string]state.RuleMode{"D": state.Exclude}
		w.lDelete("D")
		expectOps(t, w.plan(), "forget D", "forget D/a.txt")
	})
}

// synced2 adds base and local for an existing remote folder.
func (w *world) synced2(id, rel string) {
	fid := w.nextFID()
	w.s.Base = append(w.s.Base, state.Base{NodeID: id, RelPath: rel, Kind: state.KindDir, ETag: "e-" + id, FileID: fid})
	w.s.Local = append(w.s.Local, state.Local{RelPath: rel, Kind: state.KindDir, FileID: fid})
	w.paths[id] = rel
}

func TestNameProblems(t *testing.T) {
	t.Run("ignored and Windows names aren't downloaded", func(t *testing.T) {
		w := newWorld()
		w.rNew("A", "R", "x.tmp", state.KindFile, "a")
		w.rNew("B", "R", "CON.txt", state.KindFile, "b")
		w.rNew("C", "R", "ok.txt", state.KindFile, "c")
		w.s.Options.WindowsNames = true
		p := w.plan()
		expectOps(t, p, "download ok.txt")
		if got := problems(p); got != "name_windows CON.txt\nname_ignored x.tmp" {
			t.Errorf("problems %q", got)
		}
	})
	t.Run("case collision on a case-sensitive disk: the second isn't uploaded", func(t *testing.T) {
		w := newWorld()
		w.lNew("a.txt", state.KindFile, "1")
		w.lNew("A.txt", state.KindFile, "2")
		p := w.plan()
		expectOps(t, p, "upload A.txt new in R")
		if got := problems(p); got != "case_collision a.txt" {
			t.Errorf("problems %q", got)
		}
	})
	t.Run("a known file keeps its name against a new one", func(t *testing.T) {
		w := newWorld()
		w.synced("A", "R", "a.txt", state.KindFile, "1")
		w.lNew("A.txt", state.KindFile, "2")
		p := w.plan()
		expectOps(t, p)
		if got := problems(p); got != "case_collision A.txt" {
			t.Errorf("problems %q", got)
		}
	})
	t.Run("an NFD name on disk is the server's NFC name", func(t *testing.T) {
		w := newWorld()
		w.synced("A", "R", "café.txt", state.KindFile, "1")
		w.lMove("café.txt", "café.txt")
		expectOps(t, w.plan())
	})
}

func TestReadOnly(t *testing.T) {
	ro := func() *world {
		w := newWorld()
		w.synced("D", "R", "Shared", state.KindDir, "")
		w.synced("F", "D", "a.txt", state.KindFile, "v1")
		w.remote("D").Perms = state.PermRead
		w.remote("F").Perms = state.PermRead
		return w
	}
	t.Run("edit → local conflict copy, not uploaded", func(t *testing.T) {
		w := ro()
		w.lEdit("Shared/a.txt", "v2")
		expectOps(t, w.plan(), "conflict Shared/a.txt -> Shared/a (конфликт, Mac, 2026-10-25 12-00).txt")
		// The copy then stays local with a problem; the original comes back.
		w.lMove("Shared/a.txt", "Shared/a (конфликт, Mac, 2026-10-25 12-00).txt")
		for i := range w.s.Base {
			if w.s.Base[i].NodeID == "F" {
				w.s.Base[i].FileID = ""
			}
		}
		p := w.plan()
		expectOps(t, p, "download Shared/a.txt")
		if got := problems(p); got != "readonly Shared/a (конфликт, Mac, 2026-10-25 12-00).txt" {
			t.Errorf("problems %q", got)
		}
	})
	t.Run("delete → downloaded again", func(t *testing.T) {
		w := ro()
		w.lDelete("Shared/a.txt")
		expectOps(t, w.plan(), "download Shared/a.txt")
	})
}

func TestOccupiedNames(t *testing.T) {
	t.Run("a name freed by a local move waits for the PATCH", func(t *testing.T) {
		w := newWorld()
		w.synced("A", "R", "a.txt", state.KindFile, "1")
		w.lMove("a.txt", "b.txt")
		w.lNew("a.txt", state.KindFile, "2")
		p := w.plan()
		expectOps(t, p, "move_remote b.txt to R/b.txt")
		if p.Deferred != 1 {
			t.Errorf("deferred %d", p.Deferred)
		}
	})
	t.Run("a local move onto a name a new server file has → aside", func(t *testing.T) {
		w := newWorld()
		w.synced("A", "R", "a.txt", state.KindFile, "1")
		w.lMove("a.txt", "b.txt")
		w.rNew("B", "R", "b.txt", state.KindFile, "2")
		expectOps(t, w.plan(), "rename_local b.txt -> b (конфликт, Mac, 2026-10-25 12-00).txt keepbase")
	})
	t.Run("a server rename onto a new local file → the local one aside", func(t *testing.T) {
		w := newWorld()
		w.synced("A", "R", "a.txt", state.KindFile, "1")
		w.rMove("A", "R", "b.txt")
		w.lNew("b.txt", state.KindFile, "2")
		expectOps(t, w.plan(), "rename_local b.txt -> b (конфликт, Mac, 2026-10-25 12-00).txt keepbase")
	})
	t.Run("a deleted file's name for a new folder waits for the trash", func(t *testing.T) {
		w := newWorld()
		w.synced("A", "R", "x", state.KindFile, "1")
		w.lDelete("x")
		w.lNew("x", state.KindDir, "")
		p := w.plan()
		expectOps(t, p, "trash_remote x if=e-A-1")
	})
	t.Run("busy nodes are left alone", func(t *testing.T) {
		w := fileWorld()
		w.lEdit("D/a.txt", "v2")
		w.s.BusyNodes = map[string]bool{"F": true}
		if p := w.plan(); len(p.Ops) != 0 || p.Deferred != 1 {
			t.Errorf("busy: %s, deferred %d", ops(p), p.Deferred)
		}
	})
}

func TestConflictName(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	for _, c := range []struct{ in, want string }{
		{"report.docx", "report (конфликт, Mac, 2026-01-02 03-04).docx"},
		{"archive.tar.gz", "archive.tar (конфликт, Mac, 2026-01-02 03-04).gz"},
		{".bashrc", ".bashrc (конфликт, Mac, 2026-01-02 03-04)"},
		{"README", "README (конфликт, Mac, 2026-01-02 03-04)"},
	} {
		if got := conflictName(c.in, "Mac", at, 1); got != c.want {
			t.Errorf("conflictName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := conflictName("a.txt", "a/b", at, 2); got != "a (конфликт, a-b, 2026-01-02 03-04, 2).txt" {
		t.Errorf("second: %q", got)
	}
	long := strings.Repeat("я", 200) + ".txt"
	if got := conflictName(long, "Mac", at, 1); len(got) > 255 || !strings.HasSuffix(got, ").txt") {
		t.Errorf("long name: %d bytes %q", len(got), got)
	}
}

func TestTempName(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	for _, c := range []struct {
		in, want string
		n        int
	}{
		{"report.docx", "report (перенос, my.mac, 2026-01-02 03-04).docx", 1},
		{"Папка", "Папка (перенос, my.mac, 2026-01-02 03-04, 3)", 3},
		{".bashrc", ".bashrc (перенос, my.mac, 2026-01-02 03-04)", 1},
		{strings.Repeat("я", 200) + ".txt", "", 1},
	} {
		got := tempName(c.in, "my.mac", at, c.n)
		if c.want != "" && got != c.want || len(got) > 255 {
			t.Errorf("tempName(%q) = %q, want %q", c.in, got, c.want)
		}
		if !isTempName(got, "my.mac") {
			t.Errorf("%q isn't recognised", got)
		}
		if isTempName(got, "other") {
			t.Errorf("%q is taken for another device's", got)
		}
	}
	for _, name := range []string{"report.docx", "a (перенос, my.mac, вчера).txt", "a (перенос, my.mac, 2026-01-02 03-04, x)",
		"a (конфликт, my.mac, 2026-01-02 03-04).txt", "a (перенос, my.mac, 2026-01-02 03-04).txt.bak"} {
		if isTempName(name, "my.mac") {
			t.Errorf("%q taken for a temporary name", name)
		}
	}
}

// sharedWorld: «Доступно мне» with a share «Проект» (S, checked) holding
// a.txt, with perms; the own root has own.txt.
func sharedWorld(perms state.Perm) *world {
	w := newWorld()
	w.synced("own", "R", "own.txt", state.KindFile, "o")
	w.synced(SharedID, "R", SharedName, state.KindDir, "")
	w.synced("S", SharedID, "Проект", state.KindDir, "")
	w.synced("F", "S", "a.txt", state.KindFile, "v1")
	w.remote(SharedID).Perms = state.PermRead
	w.remote("S").Perms = perms
	w.remote("F").Perms = perms
	w.s.Rules["S"] = state.Include
	return w
}

// A file id freed and given again (ext4 reuses inodes at once): a share
// no longer shared still has its base row, and the user's own folder of
// the same name, re-created, got its old file id. The own folder keeps
// its node by path; the gone share takes nothing (TestSyncProperty, seed
// 9586001426729026332).
func TestReusedFileID(t *testing.T) {
	setup := func() *world {
		w := sharedWorld(state.PermRead)
		w.synced("D", "R", "Мои", state.KindDir, "")
		w.synced("X", "D", "Проект", state.KindDir, "")
		w.synced("Y", "X", "y.txt", state.KindFile, "y")
		return w
	}
	t.Run("a gone node doesn't take another's folder", func(t *testing.T) {
		w := setup()
		w.rDelete("S", "F")
		w.lDelete(SharedName + "/Проект")
		w.local("Мои/Проект").FileID = w.s.Base[2].FileID // S's
		// «Доступно мне» goes with its last share.
		expectOps(t, w.plan(), "forget "+SharedName+"/Проект", "forget "+SharedName+"/Проект/a.txt", "remove_local "+SharedName)
	})
	t.Run("a node on the server still follows its file id", func(t *testing.T) {
		// The same shape with S live: a rename of the share's folder
		// (into another one's place), undone as read-only.
		w := setup()
		w.lDelete("Мои/Проект")
		w.lMove(SharedName+"/Проект", "Мои/Проект")
		p := w.plan()
		if got := ops(p); !strings.Contains(got, "rename_local Мои/Проект -> "+SharedName+"/Проект") {
			t.Errorf("plan:\n%s", got)
		}
	})
}

func TestSharedCells(t *testing.T) {
	const a = SharedName + "/Проект/a.txt"
	const copyName = "a (конфликт, Mac, 2026-10-25 12-00).txt"
	cases := []struct {
		name   string
		perms  state.Perm
		change func(w *world)
		want   []string
		probs  string
	}{
		{"checking the root doesn't check shares", rw, func(w *world) {
			delete(w.s.Rules, "S")
		}, []string{"remove_local " + SharedName}, ""},
		{"read-only: a rename is undone", state.PermRead, func(w *world) {
			w.lMove(a, SharedName+"/Проект/b.txt")
		}, []string{"rename_local " + SharedName + "/Проект/b.txt -> " + a}, ""},
		{"read-only: moved out is a copy", state.PermRead, func(w *world) {
			w.lMove(a, "a.txt")
		}, []string{"upload a.txt new in R", "download " + a}, ""},
		{"writable: moved out is a copy and a delete", rw, func(w *world) {
			w.lMove(a, "a.txt")
		}, []string{"trash_remote " + a + " if=e-F-v1", "upload a.txt new in R"}, ""},
		{"own file moved into a read-only share comes back", state.PermRead, func(w *world) {
			w.lMove("own.txt", SharedName+"/Проект/own.txt")
		}, []string{"rename_local " + SharedName + "/Проект/own.txt -> own.txt"}, ""},
		{"own file moved into «Доступно мне» comes back", rw, func(w *world) {
			w.lMove("own.txt", SharedName+"/own.txt")
		}, []string{"rename_local " + SharedName + "/own.txt -> own.txt"}, ""},
		{"own file moved into a writable share: up there, gone here", rw, func(w *world) {
			w.lMove("own.txt", SharedName+"/Проект/own.txt")
		}, []string{"trash_remote own.txt if=e-own-o", "upload " + SharedName + "/Проект/own.txt new in S"}, ""},
		{"a share renamed locally is renamed back", rw, func(w *world) {
			w.lMove(SharedName+"/Проект", SharedName+"/Мой")
		}, []string{"rename_local " + SharedName + "/Мой -> " + SharedName + "/Проект"}, ""},
		{"«Доступно мне» renamed locally is renamed back", rw, func(w *world) {
			w.lMove(SharedName, "Другое")
		}, []string{"rename_local Другое -> " + SharedName}, ""},
		{"a deleted share comes back, not trashed", rw, func(w *world) {
			w.lDelete(SharedName + "/Проект")
		}, []string{"mkdir_local " + SharedName + "/Проект", "download " + a}, ""},
		{"new in «Доступно мне» itself: a problem", rw, func(w *world) {
			w.lNew(SharedName+"/x.txt", state.KindFile, "x")
		}, nil, "readonly " + SharedName + "/x.txt"},
		{"read-only: an edit is a conflict copy in place", state.PermRead, func(w *world) {
			w.lEdit(a, "v2")
		}, []string{"conflict " + a + " -> " + SharedName + "/Проект/" + copyName}, ""},
		{"own file gone on the server, moved into a read-only share: to the own root", state.PermRead, func(w *world) {
			w.rDelete("own")
			w.lMove("own.txt", SharedName+"/Проект/own.txt")
		}, []string{"rescue " + SharedName + "/Проект/own.txt -> own (конфликт, Mac, 2026-10-25 12-00).txt"}, ""},
		{"revoked: synced files go", rw, func(w *world) {
			w.rDelete("S", "F")
		}, []string{"remove_local " + SharedName}, ""},
		{"revoked, another share checked: only its folder goes", rw, func(w *world) {
			w.synced("T", SharedID, "Другой", state.KindDir, "")
			w.s.Rules["T"] = state.Include
			w.rDelete("S", "F")
		}, []string{"remove_local " + SharedName + "/Проект"}, ""},
		{"revoked: an edit is moved to the own tree", rw, func(w *world) {
			w.rDelete("S", "F")
			w.lEdit(a, "v2")
		}, []string{"rescue " + a + " -> " + copyName}, ""},
		{"revoked: a new folder is moved to the own tree whole", state.PermRead, func(w *world) {
			w.rDelete("S", "F")
			w.lNew(SharedName+"/Проект/N", state.KindDir, "")
			w.lNew(SharedName+"/Проект/N/c.txt", state.KindFile, "c")
		}, []string{"rescue " + SharedName + "/Проект/N -> N (конфликт, Mac, 2026-10-25 12-00)",
			"remove_local " + a}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := sharedWorld(c.perms)
			c.change(w)
			p := w.plan()
			expectOps(t, p, c.want...)
			if got := problems(p); got != c.probs {
				t.Errorf("problems %q, want %q", got, c.probs)
			}
		})
	}
}
