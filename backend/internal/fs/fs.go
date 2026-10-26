// Package fs is the domain logic of the file tree: the only code that
// changes nodes and their content. Protocol handlers (REST, WebDAV, WOPI)
// go through it and never touch the database or storage directly, so a new
// protocol can't skip a check (architecture §2).
//
// Every write keeps the database and storage in step the same way: within
// one transaction it locks the affected chain of folders, validates,
// updates metadata, then does the storage side effect last, and only then
// commits. A failed storage step rolls the transaction back; a failed
// commit undoes the storage step.
package fs

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/access"
	"github.com/do-service/cloud/backend/internal/storage"
)

var (
	ErrNotFound     = errors.New("fs: node not found")
	ErrNameConflict = errors.New("fs: a node with this name already exists")
	ErrInvalidName  = errors.New("fs: invalid name")
	ErrNotDir       = errors.New("fs: not a directory")
	ErrNotFile      = errors.New("fs: not a file")
	ErrCycle        = errors.New("fs: cannot move a folder into itself or its descendant")
	ErrIsRoot       = errors.New("fs: the root folder cannot be moved or renamed")
	// ErrForbidden: the actor sees the node but lacks the right for this.
	ErrForbidden = errors.New("fs: not allowed")
	// ErrCrossOwner: moving between trees of different owners would move
	// quota and disk paths of a whole subtree; copying does it instead.
	ErrCrossOwner = errors.New("fs: can't move between different owners' files")
)

type Kind string

const (
	KindDir  Kind = "dir"
	KindFile Kind = "file"
)

type Node struct {
	ID       string
	OwnerID  string
	ParentID string // "" for a user's root
	Name     string
	Kind     Kind
	// For a dir: total size of the subtree.
	Size        int64
	MIME        string
	ETag        string
	MTime       time.Time
	ContentHash []byte
	// Perms are the actor's rights on it, where an operation returns it
	// for a particular actor.
	Perms access.Perm

	blobID string
}

type Service struct {
	db    *pgxpool.Pool
	store storage.Backend
}

func New(db *pgxpool.Pool, store storage.Backend) *Service {
	return &Service{db: db, store: store}
}

// EnsureRoot returns the user's root folder, creating it on first use.
func (s *Service) EnsureRoot(ctx context.Context, userID string) (Node, error) {
	if s.store.Layout() == storage.LayoutTree {
		if err := s.store.Mkdir(ctx, userDirKey(userID)); err != nil {
			return Node{}, fmt.Errorf("create user dir: %w", err)
		}
	}
	if err := insertRoot(ctx, s.db, userID); err != nil {
		return Node{}, fmt.Errorf("create root: %w", err)
	}
	return getRoot(ctx, s.db, userID)
}

// Get returns a node the actor may see. Until sharing exists (E6) that is
// only their own; a foreign node is reported as missing, not forbidden, so
// ids can't be probed for existence.
func (s *Service) Get(ctx context.Context, actorID, id string) (Node, error) {
	if !validID(id) {
		return Node{}, ErrNotFound
	}
	n, err := getNode(ctx, s.db, id)
	if err != nil {
		return Node{}, err
	}
	return n, allow(ctx, s.db, actorID, &n, access.Read)
}

func (s *Service) CreateDir(ctx context.Context, actorID, parentID, name string) (Node, error) {
	return s.createDir(ctx, actorID, parentID, name, false)
}

// createDir with rename takes "name (2)" and on when the name is taken.
func (s *Service) createDir(ctx context.Context, actorID, parentID, name string, rename bool) (Node, error) {
	if !validID(parentID) {
		return Node{}, ErrNotFound
	}
	name, key, err := NormalizeName(name)
	if err != nil {
		return Node{}, err
	}
	var created Node
	err = s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		chains, err := lockChains(ctx, tx, parentID)
		if err != nil {
			return nil, err
		}
		parent, err := writableDir(ctx, tx, actorID, parentID)
		if err != nil {
			return nil, err
		}
		// A new member changes the folder.
		if err := checkLocks(ctx, tx, actorID, chains[:1], ""); err != nil {
			return nil, err
		}
		if rename {
			// The chain is locked: a free name stays free until commit.
			if name, key, err = freeName(ctx, tx, parent.ID, name); err != nil {
				return nil, err
			}
		}
		created, err = insertNode(ctx, tx, newNode{ownerID: parent.OwnerID, parentID: parent.ID, name: name, key: key, kind: KindDir})
		if err != nil {
			return nil, err
		}
		created.Perms = parent.Perms
		if err := bumpAncestors(ctx, tx, parent.ID, 0); err != nil {
			return nil, err
		}
		if err := journal(ctx, tx, parent.OwnerID, created.ID, opCreate, chains[0]); err != nil {
			return nil, err
		}
		if s.store.Layout() == storage.LayoutTree {
			// No undo: an empty directory left behind by a failed commit is
			// harmless, and removing it could take an unrelated one along.
			return nil, s.store.Mkdir(ctx, treeKey(parent.OwnerID, chains[0], name))
		}
		return nil, nil
	})
	return created, err
}

// CreateFile stores r as a new file in parentID. It never replaces: an
// existing name fails with ErrNameConflict. New versions of existing files
// come with versioning (E4-1).
func (s *Service) CreateFile(ctx context.Context, actorID, parentID, name string, r io.Reader, size int64) (Node, error) {
	// Fail before streaming possibly gigabytes into storage.
	if _, _, err := NormalizeName(name); err != nil {
		return Node{}, err
	}
	if !validID(parentID) {
		return Node{}, ErrNotFound
	}
	if _, err := writableDir(ctx, s.db, actorID, parentID); err != nil {
		return Node{}, err
	}

	blobID := newUUID()
	// The content is written before the transaction, outside any lock: an
	// upload can take hours and must not block the user's other operations.
	// In a tree layout it lands in uploads/ and is moved into place under
	// the lock; a flat layout writes the final object right away.
	stagingKey := "uploads/fs-" + blobID
	if s.store.Layout() != storage.LayoutTree {
		stagingKey = flatKey(blobID)
	}
	res, err := s.store.Put(ctx, stagingKey, r, size)
	if err != nil {
		return Node{}, fmt.Errorf("store content: %w", err)
	}

	return s.commitFile(ctx, actorID, parentID, name, staged{key: stagingKey, blobID: blobID, size: res.Size, sha256: res.SHA256}, ConflictFail, nil)
}

// staged is content already written to storage, waiting to become a file.
type staged struct {
	// key is where it is now: under uploads/ in a tree layout, the final
	// object (blobs/{blobID}) in a flat one.
	key    string
	blobID string
	size   int64
	sha256 [32]byte
	// mtime is the file's modification time to keep, as the client had
	// it; zero means now.
	mtime time.Time
}

// commitFile turns staged content into the file name in parentID: blob and
// node rows, sizes and ETags up the chain, the owner's used_bytes, and in
// a tree layout the move into place. also runs in the same transaction
// (an upload's reservation goes away with it). Whatever is left at the
// staging key on failure belongs to no node and is removed.
func (s *Service) commitFile(ctx context.Context, actorID, parentID, name string, st staged, onConflict Conflict, also func(pgx.Tx) error) (Node, error) {
	tree := s.store.Layout() == storage.LayoutTree
	backend, storageKey := "localfs", ""
	if !tree {
		backend, storageKey = "s3", st.key
	}
	var created Node
	err := s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		chains, err := lockChains(ctx, tx, parentID)
		if err != nil {
			return nil, err
		}
		parent, err := writableDir(ctx, tx, actorID, parentID)
		if err != nil {
			return nil, err
		}
		name, key, err := NormalizeName(name)
		if err != nil {
			return nil, err
		}
		if onConflict == ConflictRename {
			// The chain is locked: a free name stays free until commit.
			if name, key, err = freeName(ctx, tx, parent.ID, name); err != nil {
				return nil, err
			}
		}
		if also != nil {
			if err := also(tx); err != nil {
				return nil, err
			}
		}
		if onConflict == ConflictReplace {
			existing, err := liveChild(ctx, tx, parent.ID, key)
			switch {
			case err == nil && existing.Kind != KindFile:
				return nil, ErrNameConflict
			case err == nil:
				file := append([]chainEntry{{ID: existing.ID, Name: existing.Name}}, chains[0]...)
				if err := checkLocks(ctx, tx, actorID, [][]chainEntry{file}, ""); err != nil {
					return nil, err
				}
				created, undo, err = s.replaceContent(ctx, tx, actorID, existing, chains[0], st)
				created.Perms = parent.Perms
				return undo, err
			case !errors.Is(err, ErrNotFound):
				return nil, err
			}
		}
		if err := checkLocks(ctx, tx, actorID, chains[:1], ""); err != nil {
			return nil, err
		}
		if err := insertBlob(ctx, tx, st.blobID, backend, storageKey, st.size, st.sha256[:]); err != nil {
			return nil, err
		}
		created, err = insertNode(ctx, tx, newNode{
			ownerID: parent.OwnerID, parentID: parent.ID, name: name, key: key, kind: KindFile,
			size: st.size, mime: mimeType(name), blobID: st.blobID, contentHash: st.sha256[:], modifiedBy: authorOf(actorID), mtime: st.mtime,
		})
		if err != nil {
			return nil, err
		}
		created.Perms = parent.Perms
		if err := bumpAncestors(ctx, tx, parent.ID, st.size); err != nil {
			return nil, err
		}
		if err := addUsedBytes(ctx, tx, parent.OwnerID, st.size); err != nil {
			return nil, err
		}
		if err := journal(ctx, tx, parent.OwnerID, created.ID, opCreate, chains[0]); err != nil {
			return nil, err
		}
		if !tree {
			return nil, nil
		}
		final := treeKey(parent.OwnerID, chains[0], name)
		if err := s.store.Move(ctx, st.key, final); err != nil {
			return nil, err
		}
		return func() error { return s.store.Move(context.WithoutCancel(ctx), final, st.key) }, nil
	})
	if err != nil {
		if delErr := s.store.Delete(context.WithoutCancel(ctx), st.key); delErr != nil {
			slog.ErrorContext(ctx, "fs: remove orphaned content", "key", st.key, "err", delErr)
		}
		return Node{}, err
	}
	return created, nil
}

// Open opens a file's current content for reading; rng nil reads it all.
//
// In a tree layout the content's key is the node's path, which a
// concurrent move can change between computing it and opening the file —
// a spurious 404 at best, at worst another file that took the old path
// (risk A2). So the path is resolved and opened under shared locks on the
// node's chain, which Move and Rename take exclusively. Once open, the
// file descriptor follows the file through renames: the locks are released
// before the content is streamed.
func (s *Service) Open(ctx context.Context, actorID, id string, rng *storage.Range) (io.ReadCloser, Node, error) {
	return s.OpenRange(ctx, actorID, id, func(Node) (*storage.Range, error) { return rng, nil })
}

// OpenRange is Open with the range chosen by pick from the node's metadata,
// read in the same locked step that opens the content: a range computed
// from a size read earlier could apply to newer content. An error from
// pick (a conditional request that needs no body, say) is returned as is,
// with nothing opened.
func (s *Service) OpenRange(ctx context.Context, actorID, id string, pick func(Node) (*storage.Range, error)) (io.ReadCloser, Node, error) {
	if !validID(id) {
		return nil, Node{}, ErrNotFound
	}
	if s.store.Layout() != storage.LayoutTree {
		n, err := s.Get(ctx, actorID, id)
		if err != nil {
			return nil, Node{}, err
		}
		if n.Kind != KindFile {
			return nil, Node{}, ErrNotFile
		}
		rng, err := pick(n)
		if err != nil {
			return nil, Node{}, err
		}
		rc, err := s.store.Get(ctx, flatKey(n.blobID), rng)
		return rc, n, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, Node{}, err
	}
	defer func() { _ = rollback(tx) }()
	c, err := lockPathShared(ctx, tx, id)
	if err != nil {
		return nil, Node{}, err
	}
	n, err := getNode(ctx, tx, id)
	if err != nil {
		return nil, Node{}, err
	}
	if err := allow(ctx, tx, actorID, &n, access.Read); err != nil {
		return nil, Node{}, err
	}
	if n.Kind != KindFile {
		return nil, Node{}, ErrNotFile
	}
	rng, err := pick(n)
	if err != nil {
		return nil, Node{}, err
	}
	rc, err := s.store.Get(ctx, treeKey(n.OwnerID, c[1:], n.Name), rng)
	if err != nil {
		return nil, Node{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		rc.Close()
		return nil, Node{}, err
	}
	return rc, n, nil
}

func (s *Service) Rename(ctx context.Context, actorID, id, name string) (Node, error) {
	return s.move(ctx, actorID, id, "", name, nil)
}

// Move puts id into newParentID, under newName if it isn't "" (the current
// name otherwise).
func (s *Service) Move(ctx context.Context, actorID, id, newParentID, newName string) (Node, error) {
	return s.move(ctx, actorID, id, newParentID, newName, nil)
}

// MoveIfMatch is Move, with newParentID "" keeping the folder as Rename
// does, that fails with ErrPrecondition unless id has one of the ETags in
// ifMatch ("*": any) once locked — a sync client's If-Match. A file's ETag
// follows its content, a folder's anything inside it; neither changes
// when the node itself is moved. Empty ifMatch requires nothing.
func (s *Service) MoveIfMatch(ctx context.Context, actorID, id, newParentID, newName string, ifMatch []string) (Node, error) {
	return s.move(ctx, actorID, id, newParentID, newName, ifMatch)
}

// move with newParentID "" keeps the current parent: Rename must not race a
// concurrent move by reading the parent before taking the locks.
func (s *Service) move(ctx context.Context, actorID, id, newParentID, newName string, ifMatch []string) (Node, error) {
	if !validID(id) || newParentID != "" && !validID(newParentID) {
		return Node{}, ErrNotFound
	}
	var name, key string
	if newName != "" {
		var err error
		if name, key, err = NormalizeName(newName); err != nil {
			return Node{}, err
		}
	}
	var moved Node
	err := s.inTx(ctx, func(tx pgx.Tx) (undo func() error, err error) {
		starts := []string{id}
		if newParentID != "" {
			starts = append(starts, newParentID)
		}
		chains, err := lockChains(ctx, tx, starts...)
		if err != nil {
			return nil, err
		}
		node, err := getNode(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if err := allow(ctx, tx, actorID, &node, access.Read); err != nil {
			return nil, err
		}
		if node.ParentID == "" {
			return nil, ErrIsRoot
		}
		if err := containerWritable(ctx, tx, actorID, node.ParentID); err != nil {
			return nil, err
		}
		if !(PutCond{IfMatch: ifMatch}).holds(node, true) {
			return nil, ErrPrecondition
		}
		oldChain := chains[0][1:]
		newChain := oldChain
		if newParentID != "" {
			newChain = chains[1]
		}
		dest, err := writableDir(ctx, tx, actorID, newChain[0].ID)
		if err != nil {
			return nil, err
		}
		if dest.OwnerID != node.OwnerID {
			return nil, ErrCrossOwner
		}
		if slices.ContainsFunc(newChain, func(e chainEntry) bool { return e.ID == id }) {
			return nil, ErrCycle
		}
		if name == "" {
			name, key = node.Name, strings.ToLower(node.Name)
		}
		if newChain[0].ID == node.ParentID && name == node.Name {
			moved = node
			return nil, nil
		}
		defer func() { moved.Perms = dest.Perms }()
		// The node with all below it, and both folders' members change.
		subtree := ""
		if node.Kind == KindDir {
			subtree = id
		}
		if err := checkLocks(ctx, tx, actorID, [][]chainEntry{chains[0], oldChain, newChain}, subtree); err != nil {
			return nil, err
		}
		// Taken after the row locks, which readers never wait on, so the
		// two kinds of locks can't form a cycle.
		if err := lockPathExclusive(ctx, tx, id); err != nil {
			return nil, err
		}

		moved, err = updatePlacement(ctx, tx, id, newChain[0].ID, name, key)
		if err != nil {
			return nil, err
		}
		if newChain[0].ID == node.ParentID {
			err = bumpAncestors(ctx, tx, node.ParentID, 0)
		} else {
			err = errors.Join(
				bumpAncestors(ctx, tx, node.ParentID, -node.Size),
				bumpAncestors(ctx, tx, newChain[0].ID, node.Size),
			)
		}
		if err != nil {
			return nil, err
		}
		// One row for the subtree too: the client knows the nodes below
		// by id, and their paths follow from this one's.
		if err := journal(ctx, tx, node.OwnerID, id, opMove, chains[0], newChain); err != nil {
			return nil, err
		}
		if s.store.Layout() != storage.LayoutTree {
			return nil, nil
		}
		// One rename(2) moves the whole subtree: descendants' keys are
		// computed from the path, so no row below changes (architecture §4).
		from := treeKey(node.OwnerID, oldChain, node.Name)
		to := treeKey(node.OwnerID, newChain, name)
		if err := s.store.Move(ctx, from, to); err != nil {
			return nil, err
		}
		return func() error { return s.store.Move(context.WithoutCancel(ctx), to, from) }, nil
	})
	return moved, err
}

// inTx runs fn in a transaction. fn does its storage side effect last and
// returns how to undo it, which runs if the commit fails afterwards.
func (s *Service) inTx(ctx context.Context, fn func(pgx.Tx) (undo func() error, err error)) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	undo, err := fn(tx)
	if err != nil {
		return errors.Join(err, rollback(tx))
	}
	if err := tx.Commit(ctx); err != nil {
		if undo != nil {
			if undoErr := undo(); undoErr != nil {
				// Storage and database now disagree; the scan command
				// (Q11) reconciles them, the log says where to look.
				slog.ErrorContext(ctx, "fs: commit failed and storage change could not be undone", "commit_err", err, "undo_err", undoErr)
			}
		}
		return err
	}
	return nil
}

func rollback(tx pgx.Tx) error {
	err := tx.Rollback(context.Background())
	if errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return err
}

var errChainUnstable = errors.New("fs: folder chain kept changing while locking it")

// maxLockRounds bounds lockChains' retries; more than two rounds means
// folders keep moving under us, which only a pathological client causes.
const maxLockRounds = 4

// lockChains locks each start node and all its ancestors, and returns
// their chains as they are once locked. A chain read before locking can be
// stale (an ancestor moved meanwhile), so it is re-read until every node on
// it is already locked: a locked node can't be moved, hence the chain
// can't change anymore. Locking the chain is what keeps sizes right — a
// size delta lands on the actual ancestors — and serializes operations on
// overlapping parts of the tree.
func lockChains(ctx context.Context, tx pgx.Tx, starts ...string) ([][]chainEntry, error) {
	locked := map[string]bool{}
	for range maxLockRounds {
		chains := make([][]chainEntry, len(starts))
		var missing []string
		for i, start := range starts {
			c, err := chain(ctx, tx, start)
			if err != nil {
				return nil, err
			}
			if len(c) == 0 {
				return nil, ErrNotFound
			}
			chains[i] = c
			for _, e := range c {
				if !locked[e.ID] && !slices.Contains(missing, e.ID) {
					missing = append(missing, e.ID)
				}
			}
		}
		if len(missing) == 0 {
			return chains, nil
		}
		if err := lockNodes(ctx, tx, missing); err != nil {
			return nil, err
		}
		for _, id := range missing {
			locked[id] = true
		}
	}
	return nil, errChainUnstable
}

func writableDir(ctx context.Context, q querier, actorID, id string) (Node, error) {
	n, err := getNode(ctx, q, id)
	if err != nil {
		return Node{}, err
	}
	if err := allow(ctx, q, actorID, &n, access.Write); err != nil {
		return Node{}, err
	}
	if n.Kind != KindDir {
		return Node{}, ErrNotDir
	}
	return n, nil
}

// containerWritable checks the actor may change the folder holding a node
// they can see: renaming, moving or deleting the node changes that folder.
// A recipient sees a shared folder but not its parent, which makes acting
// on the shared folder itself forbidden, not "not found".
func containerWritable(ctx context.Context, q querier, actorID, parentID string) error {
	_, err := writableDir(ctx, q, actorID, parentID)
	if errors.Is(err, ErrNotFound) {
		return ErrForbidden
	}
	return err
}

// allow checks that actorID has need on n, filling in n.Perms. A node the
// actor has no right on is ErrNotFound, not ErrForbidden, so ids can't be
// probed for existence.
func allow(ctx context.Context, q querier, actorID string, n *Node, need access.Perm) error {
	p, err := access.Perms(ctx, q, actorID, n.ID, n.OwnerID)
	if err != nil {
		return fmt.Errorf("check access: %w", err)
	}
	n.Perms = p
	switch {
	// No right at all: the node doesn't exist as far as the actor knows.
	case p == access.None:
		return ErrNotFound
	// Some, not this one. An upload link has write without read: it may
	// put files in a folder it can't list.
	case !p.Has(need):
		return ErrForbidden
	}
	return nil
}

// ownerOnly is for what only the owner sees: the trash, the quota.
func ownerOnly(actorID string, n Node) error {
	if n.OwnerID != actorID {
		return ErrNotFound
	}
	return nil
}

func userDirKey(userID string) string { return "files/" + userID }

func flatKey(blobID string) string { return "blobs/" + blobID }

// PreviewKey is where a blob's preview of size is stored; with size ""
// the folder of all its previews.
func PreviewKey(blobID, size string) string {
	if size == "" {
		return "previews/" + blobID
	}
	return "previews/" + blobID + "/" + size + ".webp"
}

// BlobID is the content the node has now: a new version, a new blob.
// Derived data such as previews is keyed by it.
func (n Node) BlobID() string { return n.blobID }

// treeKey is the storage key of name inside the folder whose chain (folder
// first, root last) is given.
func treeKey(ownerID string, folder []chainEntry, name string) string {
	parts := []string{userDirKey(ownerID)}
	// The root's name is empty; it is the user dir itself.
	for i := len(folder) - 2; i >= 0; i-- {
		parts = append(parts, folder[i].Name)
	}
	return path.Join(append(parts, name)...)
}

func mimeType(name string) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func newUUID() string {
	var b [16]byte
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
