-- +goose Up
-- WebDAV dead properties (RFC 4918 §4): whatever clients PROPPATCH onto a
-- node — Windows its Win32 timestamps, macOS its Finder data — kept as
-- given and returned by PROPFIND. They follow the node through moves and
-- go with it when it is purged.
CREATE TABLE dav_props (
    node_id uuid NOT NULL REFERENCES nodes ON DELETE CASCADE,
    namespace text NOT NULL,
    name text NOT NULL,
    -- The property's content as self-contained XML: any namespace it uses
    -- is declared inside it.
    value text NOT NULL,
    PRIMARY KEY (node_id, namespace, name)
);

-- +goose Down
DROP TABLE dav_props;
