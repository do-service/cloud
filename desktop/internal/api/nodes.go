package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// RootID stands for the user's own root folder wherever an id goes.
const RootID = "root"

type PathEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// NodeWithPath is a node and the folders leading to it.
type NodeWithPath struct {
	Node
	// Path runs from the top of what the user sees (the root, named "")
	// down to the node itself.
	Path []PathEntry
}

// Get reads one node and its path.
func (c *Client) Get(ctx context.Context, id string) (NodeWithPath, error) {
	// Two passes over one body: Node's UnmarshalJSON would swallow the
	// path if Node were embedded in the target.
	var body json.RawMessage
	if err := c.do(ctx, call{method: http.MethodGet, path: nodePath(id), out: &body}); err != nil {
		return NodeWithPath{}, err
	}
	var n NodeWithPath
	var path struct {
		Path []PathEntry `json:"path"`
	}
	if err := json.Unmarshal(body, &n.Node); err != nil {
		return NodeWithPath{}, err
	}
	if err := json.Unmarshal(body, &path); err != nil {
		return NodeWithPath{}, err
	}
	n.Path = path.Path
	return n, nil
}

// Mkdir creates folder name in parentID. A taken name is ErrNameConflict.
func (c *Client) Mkdir(ctx context.Context, parentID, name string) (Node, error) {
	var n Node
	err := c.do(ctx, call{method: http.MethodPost, path: nodePath(parentID) + "/children", in: map[string]string{"name": name}, out: &n})
	return n, err
}

// Move renames and/or moves node id: into parentID unless it's "", under
// name unless it's "". With ifMatch (the ETag the client last saw) only
// if the node still has it, else ErrPrecondition. A rename or move
// doesn't change a node's ETag, so the old one stays the base.
func (c *Client) Move(ctx context.Context, id, parentID, name, ifMatch string) (Node, error) {
	body := map[string]string{}
	if parentID != "" {
		body["parentId"] = parentID
	}
	if name != "" {
		body["name"] = name
	}
	var n Node
	err := c.do(ctx, call{method: http.MethodPatch, path: nodePath(id), in: body, out: &n, header: ifMatchHeader(ifMatch)})
	return n, err
}

// Trash moves node id to the trash, with ifMatch as in Move.
//
// A DELETE is retried after a network failure; if the first attempt did
// go through, the retry answers ErrNotFound: for the caller the node is
// gone either way.
func (c *Client) Trash(ctx context.Context, id, ifMatch string) error {
	return c.do(ctx, call{method: http.MethodDelete, path: nodePath(id), header: ifMatchHeader(ifMatch)})
}

func nodePath(id string) string { return "/api/v1/nodes/" + url.PathEscape(id) }

func ifMatchHeader(etag string) http.Header {
	if etag == "" {
		return nil
	}
	return http.Header{"If-Match": {quoteETag(etag)}}
}
