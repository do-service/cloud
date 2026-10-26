package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/localfs"
)

// fakeServer is the server's sync contract in memory, for the property
// test: names compared by key and refused like fs.NormalizeName does, a
// file's ETag following its content and a folder's anything inside it,
// If-Match, the journal as a dirty set (one row per node changed, the
// subtree of a moved or trashed folder not journaled, tombstones), 410
// for a cursor older than the journal, and a trash and versions that
// keep every content the server ever had. Other owners' trees exist too
// (addOwner), shared with the client's user u1 by node, with rights
// (share): /sync/roots lists them, a share's change says rootsChanged,
// writes need the rights, nothing moves between owners, quota is the
// owner's.
type fakeServer struct {
	mu     sync.Mutex
	nodes  map[string]*fnode
	rootID string
	// names of the owners, by id; shares to u1 by node, in the order
	// made; shareVer changes with any of them.
	names    map[string]string
	shares   map[string]*fshare
	shareN   int
	shareVer int
	// quota of an owner, bytes of live files; none if absent.
	quota  map[string]int64
	seq    int64
	minSeq int64
	jseq   []int64
	jid    []string
	nextID int
	// blobs has every content ever uploaded, by SHA-256: nothing the
	// server had is lost (versions and trash keep it all).
	blobs map[string][]byte
	// fault, when set, decides for a call whether it fails before doing
	// anything or after doing it (the answer lost).
	fault func(method string) (before, after bool)
	calls int
}

type fshare struct {
	perms []string
	order int
}

type fnode struct {
	id, parent, name string
	owner            string
	dir              bool
	sha              string
	size             int64
	etag             string
	mtime            time.Time
	trashed          bool
}

func newFakeServer() *fakeServer {
	s := &fakeServer{nodes: map[string]*fnode{}, blobs: map[string][]byte{}, names: map[string]string{},
		shares: map[string]*fshare{}, quota: map[string]int64{}}
	s.rootID = s.addOwnerLocked("u1", "u")
	return s
}

func (s *fakeServer) addOwnerLocked(id, name string) string {
	root := s.newID()
	s.nodes[root] = &fnode{id: root, owner: id, dir: true, etag: s.newETag(), mtime: time.Unix(1_700_000_000, 0)}
	s.names[id] = name
	return root
}

// addOwner makes another user's tree and returns its root.
func (s *fakeServer) addOwner(id, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addOwnerLocked(id, name)
}

// share shares node id with u1 (again: new rights); unshare revokes it.
func (s *fakeServer) share(id string, perms ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sh := s.shares[id]; sh != nil {
		sh.perms = perms
	} else {
		s.shareN++
		s.shares[id] = &fshare{perms: perms, order: s.shareN}
	}
	s.shareVer++
}

func (s *fakeServer) unshare(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.shares, id)
	s.shareVer++
}

var allPerms = []string{"read", "write", "reshare"}

// access is what u1 may do with a live node: their own tree, or what is
// shared with them, with the share's rights; top: a shared node whose
// folder they can't see.
func (s *fakeServer) access(id string) (perms []string, top, ok bool) {
	n := s.live(id)
	if n == nil {
		return nil, false, false
	}
	for cur := n; cur != nil; cur = s.nodes[cur.parent] {
		if cur.id == s.rootID {
			return allPerms, false, true
		}
		if sh := s.shares[cur.id]; sh != nil {
			if cur == n {
				_, _, above := s.access(n.parent)
				return sh.perms, !above, true
			}
			return sh.perms, false, true
		}
	}
	return nil, false, false
}

func (s *fakeServer) visible(id string) bool {
	_, _, ok := s.access(id)
	return ok
}

// writable: u1 may write into (or to) the node.
func (s *fakeServer) writable(id string) bool {
	perms, _, ok := s.access(id)
	return ok && slices.Contains(perms, "write")
}

// overQuota: adding size bytes to owner's files goes past their quota.
func (s *fakeServer) overQuota(owner string, size int64) bool {
	q, ok := s.quota[owner]
	if !ok {
		return false
	}
	used := int64(0)
	for _, n := range s.nodes {
		if n.owner == owner && !n.dir && s.live(n.id) != nil {
			used += n.size
		}
	}
	return used+size > q
}

var errNetwork = &url.Error{Op: "Post", URL: "http://fake", Err: errors.New("connection reset by peer")}

func apiErr(status int, code string) error {
	return &api.APIError{Status: status, Code: code, Message: code}
}

func (s *fakeServer) newID() string { s.nextID++; return fmt.Sprintf("n%04d", s.nextID) }
func (s *fakeServer) newETag() string {
	s.nextID++
	return fmt.Sprintf("e%d", s.nextID)
}

func (s *fakeServer) journal(id string) {
	s.seq++
	s.jseq = append(s.jseq, s.seq)
	s.jid = append(s.jid, id)
}

// live: not trashed, nor anything above it up to its owner's root.
func (s *fakeServer) live(id string) *fnode {
	n := s.nodes[id]
	for cur := n; cur != nil; cur = s.nodes[cur.parent] {
		if cur.trashed {
			return nil
		}
		if cur.parent == "" {
			return n
		}
	}
	return nil
}

func (s *fakeServer) bump(dirID string) {
	for cur := s.nodes[dirID]; cur != nil; cur = s.nodes[cur.parent] {
		cur.etag = s.newETag()
		if cur.parent == "" {
			return
		}
	}
}

func (s *fakeServer) child(parent, key string, except string) *fnode {
	for _, n := range s.nodes {
		if n.parent == parent && n.id != except && !n.trashed && n.id != s.rootID && localfs.NameKey(n.name) == key {
			return n
		}
	}
	return nil
}

func (s *fakeServer) toNode(n *fnode) api.Node {
	perms, top, _ := s.access(n.id)
	out := api.Node{ID: n.id, Name: n.name, Kind: api.KindFile, Size: n.size, ETag: n.etag, SHA256: n.sha, MTime: n.mtime,
		Permissions: slices.Clone(perms)}
	if n.dir {
		out.Kind = api.KindDir
		var size int64
		for _, c := range s.nodes {
			if c.parent == n.id && s.live(c.id) != nil && !c.dir {
				size += c.size
			}
		}
		out.Size = size
	}
	if n.parent != "" && !top {
		out.ParentID = n.parent
	}
	return out
}

// enter applies the fault policy; done is called after the action.
func (s *fakeServer) enter(method string) (before, after bool) {
	s.calls++
	if s.fault == nil {
		return false, false
	}
	return s.fault(method)
}

func (s *fakeServer) Roots(ctx context.Context) (api.Roots, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, _ := s.enter("roots"); b {
		return api.Roots{}, errNetwork
	}
	r := s.nodes[s.rootID]
	out := api.Roots{Cursor: s.cursor(), Roots: []api.Root{{ID: s.rootID, Kind: "own", ETag: r.etag,
		Owner: api.Owner{ID: "u1", Name: "u"}, Permissions: allPerms}}}
	// Named as share.Entries names them: by the owner where names
	// repeat, then by a number.
	var ids []string
	for id := range s.shares {
		if _, top, ok := s.access(id); ok && top {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return s.shares[ids[i]].order < s.shares[ids[j]].order })
	count := map[string]int{}
	for _, id := range ids {
		count[localfs.NameKey(s.nodes[id].name)]++
	}
	taken := map[string]bool{}
	for _, id := range ids {
		n := s.nodes[id]
		name := n.name
		if count[localfs.NameKey(name)] > 1 {
			name = fmt.Sprintf("%s (%s)", n.name, s.names[n.owner])
		}
		base := name
		for i := 2; taken[localfs.NameKey(name)]; i++ {
			name = fmt.Sprintf("%s (%d)", base, i)
		}
		taken[localfs.NameKey(name)] = true
		out.Roots = append(out.Roots, api.Root{ID: id, Kind: "shared", Name: name, ETag: n.etag,
			Owner: api.Owner{ID: n.owner, Name: s.names[n.owner]}, Permissions: s.shares[id].perms})
	}
	return out, nil
}

// cursor is "seq.shareVer": a change of the shares since says
// rootsChanged.
func (s *fakeServer) cursor() string { return fmt.Sprintf("%d.%d", s.seq, s.shareVer) }

func (s *fakeServer) Tree(ctx context.Context, root string, fn func(api.Node) error) (string, error) {
	s.mu.Lock()
	if b, _ := s.enter("tree"); b {
		s.mu.Unlock()
		return "", errNetwork
	}
	if root == api.RootID {
		root = s.rootID
	}
	if !s.visible(root) {
		s.mu.Unlock()
		return "", apiErr(http.StatusNotFound, api.CodeNotFound)
	}
	var out []api.Node
	queue := []string{root}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		n := s.nodes[id]
		out = append(out, s.toNode(n))
		var kids []string
		for _, c := range s.nodes {
			if c.parent == id && !c.trashed && c.id != s.rootID {
				kids = append(kids, c.id)
			}
		}
		sort.Strings(kids)
		queue = append(queue, kids...)
	}
	cursor := s.cursor()
	s.mu.Unlock()
	for _, n := range out {
		if err := fn(n); err != nil {
			return "", err
		}
	}
	return cursor, nil
}

func (s *fakeServer) Changes(ctx context.Context, cursor string, limit int, wait time.Duration) (api.Changes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, _ := s.enter("changes"); b {
		return api.Changes{}, errNetwork
	}
	seqPart, verPart, _ := strings.Cut(cursor, ".")
	from, err := strconv.ParseInt(seqPart, 10, 64)
	ver, verr := strconv.Atoi(verPart)
	if err != nil || verr != nil {
		return api.Changes{}, apiErr(http.StatusBadRequest, "invalid_request")
	}
	if from < s.minSeq || from > s.seq {
		return api.Changes{}, apiErr(http.StatusGone, api.CodeCursorExpired)
	}
	var order []string
	seen := map[string]bool{}
	for i, q := range s.jseq {
		if q > from && !seen[s.jid[i]] {
			seen[s.jid[i]] = true
			order = append(order, s.jid[i])
		}
	}
	out := api.Changes{Changes: []api.Change{}, Cursor: s.cursor(), RootsChanged: ver != s.shareVer}
	done := map[string]bool{}
	var emit func(id string)
	emit = func(id string) {
		if done[id] {
			return
		}
		done[id] = true
		n := s.live(id)
		if p := n.parent; seen[p] && !done[p] && s.visible(p) {
			emit(p)
		}
		node := s.toNode(n)
		// In someone else's tree a folder may come from where the user
		// couldn't see it: read its tree (the server says so for moved
		// ones; here for any).
		out.Changes = append(out.Changes, api.Change{ID: id, Owner: n.owner, Node: &node, Subtree: n.dir && n.owner != "u1"})
	}
	for _, id := range order {
		if s.visible(id) {
			emit(id)
		}
	}
	for _, id := range order {
		if !s.visible(id) {
			out.Changes = append(out.Changes, api.Change{ID: id, Owner: s.nodes[id].owner, Deleted: true})
		}
	}
	return out, nil
}

func (s *fakeServer) Get(ctx context.Context, id string) (api.NodeWithPath, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, _ := s.enter("get"); b {
		return api.NodeWithPath{}, errNetwork
	}
	n := s.live(id)
	if n == nil || !s.visible(id) {
		return api.NodeWithPath{}, apiErr(http.StatusNotFound, api.CodeNotFound)
	}
	return api.NodeWithPath{Node: s.toNode(n)}, nil
}

func (s *fakeServer) Mkdir(ctx context.Context, parentID, name string) (api.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before, after := s.enter("mkdir")
	if before {
		return api.Node{}, errNetwork
	}
	if err := s.canWrite(parentID); err != nil {
		return api.Node{}, err
	}
	n, err := s.mkdir(parentID, name)
	if err != nil {
		return api.Node{}, err
	}
	if after {
		return api.Node{}, errNetwork
	}
	return s.toNode(n), nil
}

func (s *fakeServer) mkdir(parentID, name string) (*fnode, error) {
	p := s.live(parentID)
	if p == nil || !p.dir {
		return nil, apiErr(http.StatusNotFound, api.CodeNotFound)
	}
	nm, key, err := localfs.NormalizeName(name)
	if err != nil {
		return nil, apiErr(http.StatusBadRequest, "invalid_name")
	}
	if s.child(parentID, key, "") != nil {
		return nil, apiErr(http.StatusConflict, api.CodeNameConflict)
	}
	n := &fnode{id: s.newID(), parent: parentID, owner: p.owner, name: nm, dir: true, etag: s.newETag(), mtime: time.Unix(1_700_000_000, 0)}
	s.nodes[n.id] = n
	s.bump(parentID)
	s.journal(n.id)
	return n, nil
}

func (s *fakeServer) Move(ctx context.Context, id, parentID, name, ifMatch string) (api.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before, after := s.enter("move")
	if before {
		return api.Node{}, errNetwork
	}
	if n := s.nodes[id]; n != nil && s.visible(id) {
		if err := s.canWrite(n.parent); err != nil {
			return api.Node{}, err
		}
		if parentID != "" {
			if err := s.canWrite(parentID); err != nil {
				return api.Node{}, err
			}
			if p := s.nodes[parentID]; p.owner != n.owner {
				return api.Node{}, apiErr(http.StatusConflict, "cross_owner_move")
			}
		}
	} else {
		return api.Node{}, apiErr(http.StatusNotFound, api.CodeNotFound)
	}
	n, err := s.move(id, parentID, name, ifMatch)
	if err != nil {
		return api.Node{}, err
	}
	if after {
		return api.Node{}, errNetwork
	}
	return s.toNode(n), nil
}

func (s *fakeServer) move(id, parentID, name, ifMatch string) (*fnode, error) {
	n := s.live(id)
	if n == nil || id == s.rootID {
		return nil, apiErr(http.StatusNotFound, api.CodeNotFound)
	}
	if ifMatch != "" && ifMatch != n.etag {
		return nil, apiErr(http.StatusPreconditionFailed, api.CodePreconditionFailed)
	}
	if parentID == "" {
		parentID = n.parent
	}
	p := s.live(parentID)
	if p == nil || !p.dir {
		return nil, apiErr(http.StatusNotFound, api.CodeNotFound)
	}
	for cur := p; cur != nil; cur = s.nodes[cur.parent] {
		if cur.id == id {
			return nil, apiErr(http.StatusConflict, "cycle")
		}
		if cur.id == s.rootID {
			break
		}
	}
	if name == "" {
		name = n.name
	}
	nm, key, err := localfs.NormalizeName(name)
	if err != nil {
		return nil, apiErr(http.StatusBadRequest, "invalid_name")
	}
	if parentID == n.parent && nm == n.name {
		return n, nil
	}
	if s.child(parentID, key, id) != nil {
		return nil, apiErr(http.StatusConflict, api.CodeNameConflict)
	}
	old := n.parent
	n.parent, n.name = parentID, nm
	s.bump(old)
	s.bump(parentID)
	s.journal(id)
	return n, nil
}

func (s *fakeServer) Trash(ctx context.Context, id, ifMatch string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before, after := s.enter("trash")
	if before {
		return errNetwork
	}
	if n := s.nodes[id]; n == nil || !s.visible(id) {
		return apiErr(http.StatusNotFound, api.CodeNotFound)
	} else if err := s.canWrite(n.parent); err != nil {
		return err
	}
	if err := s.trash(id, ifMatch); err != nil {
		return err
	}
	if after {
		return errNetwork
	}
	return nil
}

func (s *fakeServer) trash(id, ifMatch string) error {
	n := s.live(id)
	if n == nil || id == s.rootID {
		return apiErr(http.StatusNotFound, api.CodeNotFound)
	}
	if ifMatch != "" && ifMatch != n.etag {
		return apiErr(http.StatusPreconditionFailed, api.CodePreconditionFailed)
	}
	n.trashed = true
	s.bump(n.parent)
	s.journal(id)
	return nil
}

func (s *fakeServer) DownloadFile(ctx context.Context, id, path string, opts api.DownloadOptions) (api.DownloadResult, error) {
	s.mu.Lock()
	if b, _ := s.enter("download"); b {
		s.mu.Unlock()
		return api.DownloadResult{}, errNetwork
	}
	n := s.live(id)
	if n == nil || n.dir || !s.visible(id) {
		s.mu.Unlock()
		return api.DownloadResult{}, apiErr(http.StatusNotFound, api.CodeNotFound)
	}
	body, etag, sha := s.blobs[n.sha], n.etag, n.sha
	s.mu.Unlock()
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return api.DownloadResult{}, err
	}
	return api.DownloadResult{ETag: etag, Size: int64(len(body)), SHA256: sha}, nil
}

func (s *fakeServer) Upload(ctx context.Context, u api.Upload) (api.UploadResult, error) {
	body, err := io.ReadAll(io.NewSectionReader(u.Body, 0, u.Size))
	if err != nil {
		return api.UploadResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before, after := s.enter("upload")
	if before {
		return api.UploadResult{}, errNetwork
	}
	target := u.ParentID
	if u.NodeID != "" {
		target = u.NodeID
	}
	if err := s.canWrite(target); err != nil {
		return api.UploadResult{}, err
	}
	if t := s.nodes[target]; s.overQuota(t.owner, int64(len(body))) {
		return api.UploadResult{}, apiErr(http.StatusInsufficientStorage, api.CodeQuotaExceeded)
	}
	n, err := s.upload(u, body)
	if err != nil {
		return api.UploadResult{}, err
	}
	if after {
		return api.UploadResult{}, errNetwork
	}
	return api.UploadResult{NodeID: n.id, ETag: n.etag}, nil
}

func (s *fakeServer) upload(u api.Upload, body []byte) (*fnode, error) {
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	if u.SHA256 != "" && !strings.EqualFold(u.SHA256, sha) {
		return nil, apiErr(http.StatusBadRequest, api.CodeChecksumMismatch)
	}
	if u.NodeID != "" {
		n := s.live(u.NodeID)
		if n == nil || n.dir {
			return nil, apiErr(http.StatusNotFound, api.CodeNotFound)
		}
		if u.IfMatch != "" && u.IfMatch != n.etag {
			return nil, apiErr(http.StatusPreconditionFailed, api.CodePreconditionFailed)
		}
		s.blobs[sha] = body
		n.sha, n.size, n.etag, n.mtime = sha, int64(len(body)), s.newETag(), u.MTime
		s.bump(n.parent)
		s.journal(n.id)
		return n, nil
	}
	p := s.live(u.ParentID)
	if p == nil || !p.dir {
		return nil, apiErr(http.StatusNotFound, api.CodeNotFound)
	}
	nm, key, err := localfs.NormalizeName(u.Name)
	if err != nil {
		return nil, apiErr(http.StatusBadRequest, "invalid_name")
	}
	if s.child(u.ParentID, key, "") != nil {
		return nil, apiErr(http.StatusConflict, api.CodeNameConflict)
	}
	s.blobs[sha] = body
	n := &fnode{id: s.newID(), parent: u.ParentID, owner: p.owner, name: nm, sha: sha, size: int64(len(body)), etag: s.newETag(), mtime: u.MTime}
	s.nodes[n.id] = n
	s.bump(u.ParentID)
	s.journal(n.id)
	return n, nil
}

// expire drops the journal behind the head: older cursors get 410.
func (s *fakeServer) expire() {
	s.mu.Lock()
	s.minSeq = s.seq
	s.mu.Unlock()
}

// canWrite: what u1's own writes check, like the server's access: a node
// they can't see is 404, one they can't write to 403.
func (s *fakeServer) canWrite(id string) error {
	if !s.visible(id) {
		return apiErr(http.StatusNotFound, api.CodeNotFound)
	}
	if !s.writable(id) {
		return apiErr(http.StatusForbidden, api.CodeForbidden)
	}
	return nil
}

// put creates or replaces a file at a path of names, as another client
// would (no faults).
func (s *fakeServer) put(path, content string) { s.putIn(s.rootID, path, content) }

// putIn is put in the tree below folder top, as its owner would.
func (s *fakeServer) putIn(top, path, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent := top
	parts := strings.Split(path, "/")
	for _, p := range parts[:len(parts)-1] {
		c := s.child(parent, localfs.NameKey(p), "")
		if c == nil {
			var err error
			if c, err = s.mkdir(parent, p); err != nil {
				return
			}
		}
		if !c.dir {
			return
		}
		parent = c.id
	}
	name := parts[len(parts)-1]
	u := api.Upload{ParentID: parent, Name: name, MTime: time.Unix(1_700_000_000, 0)}
	if c := s.child(parent, localfs.NameKey(name), ""); c != nil {
		if c.dir {
			return
		}
		u = api.Upload{NodeID: c.id, MTime: u.MTime}
	}
	_, _ = s.upload(u, []byte(content))
}

// liveFiles is every live node by path (names as the server has them).
func (s *fakeServer) tree() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	var walk func(id, prefix string)
	walk = func(id, prefix string) {
		for _, c := range s.nodes {
			if c.parent != id || c.trashed || c.id == s.rootID {
				continue
			}
			p := join(prefix, c.name)
			if c.dir {
				out[p] = "/"
				walk(c.id, p)
			} else {
				out[p] = c.sha
			}
		}
	}
	walk(s.rootID, "")
	return out
}

func (s *fakeServer) has(sha string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.blobs[sha]
	return ok
}

// liveIDs lists the live nodes of owner's tree, but its root.
func (s *fakeServer) liveIDs() []string { return s.liveIn("u1") }

func (s *fakeServer) liveIn(owner string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, n := range s.nodes {
		if n.parent != "" && n.owner == owner && s.live(id) != nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
