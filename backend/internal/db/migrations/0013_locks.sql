-- +goose Up
-- Write locks (R11, R12): a WebDAV client's LOCK or an editor's WOPI lock
-- on a node. While one holds, the node can't be written through any
-- protocol but by its owner presenting the token, so a file open in an
-- editor isn't overwritten from a sync client or the web UI. Exclusive
-- only: one lock per node.
CREATE TABLE locks (
    node_id uuid PRIMARY KEY REFERENCES nodes ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('webdav', 'wopi')),
    token text NOT NULL UNIQUE,
    owner_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    -- deep: covers everything below a folder too (WebDAV Depth infinity).
    deep boolean NOT NULL DEFAULT false,
    -- The WebDAV <owner> as the client sent it, shown back in lockdiscovery.
    owner_info text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

-- +goose Down
DROP TABLE locks;
