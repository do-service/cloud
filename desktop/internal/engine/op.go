package engine

import (
	"github.com/do-service/cloud/desktop/internal/localfs"
	"github.com/do-service/cloud/desktop/internal/state"
)

// OpKind is what an Op does; it is stored as state.Op.Kind.
type OpKind string

const (
	// Base-only: no action outside the state database.
	OpSetBase OpKind = "set_base" // link two equal sides, refresh a stamp or ETag
	OpRebase  OpKind = "rebase"   // both sides moved to the same place
	OpForget  OpKind = "forget"   // both sides deleted

	// Structure, run one by one, parents first.
	OpMkdirRemote OpKind = "mkdir_remote"
	OpMoveRemote  OpKind = "move_remote"
	OpMkdirLocal  OpKind = "mkdir_local"
	OpRenameLocal OpKind = "rename_local"
	// OpConflict moves an edited file aside under a conflict name.
	OpConflict OpKind = "conflict"
	// OpRescue moves what the user wrote in a share that is gone (access
	// revoked, deleted by its owner) into their own tree, to be uploaded
	// there; base forgets the node.
	OpRescue OpKind = "rescue"

	// Deletes, children before parents.
	OpTrashRemote OpKind = "trash_remote"
	OpRemoveLocal OpKind = "remove_local"

	// Transfers, in parallel.
	OpUpload   OpKind = "upload"
	OpDownload OpKind = "download"
)

// phase orders a pass: base updates, then structure (so transfers find
// their folders), deletes (after moves have taken out what stays), then
// transfers.
func (k OpKind) phase() int {
	switch k {
	case OpSetBase, OpRebase, OpForget:
		return 0
	case OpMkdirRemote, OpMoveRemote, OpMkdirLocal, OpRenameLocal, OpConflict, OpRescue:
		return 1
	case OpTrashRemote, OpRemoveLocal:
		return 2
	}
	return 3
}

// local: the op changes the sync folder, so its result is recorded only
// once the change is durable (localfs.Syncer).
func (k OpKind) local() bool {
	switch k {
	case OpDownload, OpMkdirLocal, OpRenameLocal, OpConflict, OpRescue, OpRemoveLocal:
		return true
	}
	return false
}

// journaled: the op acts outside the database, so it goes through the
// journal (Intend → act → Complete).
func (k OpKind) journaled() bool { return k.phase() > 0 }

// Op is one step of a plan. Kind, NodeID and Path go to the journal's
// columns, the rest is its params (JSON).
type Op struct {
	Kind   OpKind `json:"-"`
	NodeID string `json:"-"`
	// Path is the local path the op ends at (relative, slash-separated).
	Path string `json:"-"`

	// From is where a local rename starts.
	From string `json:"from,omitempty"`
	// BasePath is the node's path in base before the op: a move re-paths
	// base from it, with the subtree.
	BasePath string `json:"basePath,omitempty"`
	// KeepBase: a rename that base doesn't follow, to free a name; the
	// node's position then differs locally and the next pass moves it on
	// the server.
	KeepBase bool `json:"keepBase,omitempty"`

	// ParentID and Name are the remote place of a create or move.
	ParentID string `json:"parentId,omitempty"`
	Name     string `json:"name,omitempty"`
	IfMatch  string `json:"ifMatch,omitempty"`

	// Content of a transfer: what is uploaded (from the local hash) or
	// expected from the server.
	ETag    string `json:"etag,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Size    int64  `json:"size,omitempty"`
	MtimeNS int64  `json:"mtimeNs,omitempty"`
	// Expect is the local file's stamp the op was planned on: an upload
	// sends it only if the file still has it, a download or remove
	// replaces it only then; nil for a download that creates the file.
	Expect *localfs.Stamp `json:"expect,omitempty"`

	// Include: a node the user created locally gets an include rule, so a
	// container folder doesn't drop it.
	Include bool `json:"include,omitempty"`
	// Forget is a base node the op's result replaces (re-uploaded after a
	// remote delete, a folder re-created).
	Forget string `json:"forget,omitempty"`
	// Dir: a folder (trash: with everything below it).
	Dir bool `json:"dir,omitempty"`
	// Items of a local remove, deepest first.
	Items []RemoveItem `json:"items,omitempty"`
	// Base is the new base row of OpSetBase.
	Base *state.Base `json:"base,omitempty"`

	// Deletes counts the files the op makes disappear on the other side
	// for the mass-delete guard: files trashed on the server for local
	// deletes, files removed locally for server deletes.
	Deletes int `json:"-"`
}

// RemoveItem is one entry of a local remove: removed only while it still
// has Expect, its base row dropped with it.
type RemoveItem struct {
	Path   string        `json:"path"`
	NodeID string        `json:"nodeId"`
	Expect localfs.Stamp `json:"expect"`
}

// key identifies the op's target for backoff and busy checks.
func (o Op) key() string {
	if o.NodeID != "" {
		return "n:" + o.NodeID
	}
	return "p:" + o.Path
}
