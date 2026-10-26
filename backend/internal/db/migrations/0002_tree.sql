-- +goose Up
-- File names sort the way Finder and Explorer do it: "file2" before
-- "file10", Cyrillic (ё included) in alphabet order. Pinned to ICU instead
-- of the database default because the default is libc's and differs
-- between hosts — musl (the alpine image) compares bytes, glibc doesn't —
-- and a glibc upgrade can silently corrupt indexes built on it. ICU
-- versions its collations, so Postgres warns when a REINDEX is due.
-- Deterministic: names that differ only in bytes still compare unequal,
-- which the unique index on name_key relies on.
CREATE COLLATION name_order (provider = icu, locale = 'und-u-kn-true');

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username      citext NOT NULL UNIQUE,
    email         citext,
    display_name  text NOT NULL DEFAULT '',
    -- NULL for users who only sign in through OIDC.
    password_hash text,
    is_admin      boolean NOT NULL DEFAULT false,
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    -- NULL means unlimited.
    quota_bytes   bigint CHECK (quota_bytes >= 0),
    -- Current files plus versions; kept by the fs service, not derived
    -- from the root's size, which only covers current files.
    used_bytes    bigint NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE blobs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    backend     text NOT NULL CHECK (backend IN ('localfs', 's3')),
    -- NULL only for a file's current content on localfs: that key is the
    -- node's path and is computed, so moving a folder doesn't rewrite a
    -- row per descendant (architecture §4).
    storage_key text,
    size        bigint NOT NULL CHECK (size >= 0),
    hash        bytea NOT NULL CHECK (length(hash) = 32), -- SHA-256
    refcount    integer NOT NULL DEFAULT 1 CHECK (refcount >= 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    CHECK (backend = 'localfs' OR storage_key IS NOT NULL)
);

CREATE TABLE nodes (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Whose tree and quota this is, even when a share recipient created it.
    owner_id     uuid NOT NULL REFERENCES users,
    -- NULL for the user's root. Cascade so purging a trashed folder is
    -- one DELETE; files on disk are removed by the fs service, not here.
    parent_id    uuid REFERENCES nodes ON DELETE CASCADE,
    name         text NOT NULL,
    -- NFC + lower-case name, computed by the fs service: uniqueness and
    -- sort order, so clients on case-insensitive filesystems never see two
    -- names they can't tell apart (risk A4).
    name_key     text COLLATE name_order NOT NULL,
    kind         text NOT NULL CHECK (kind IN ('dir', 'file')),
    -- For a dir: total size of the subtree, kept up to date up the
    -- ancestor chain.
    size         bigint NOT NULL DEFAULT 0 CHECK (size >= 0),
    mime         text,
    blob_id      uuid REFERENCES blobs,
    content_hash bytea CHECK (length(content_hash) = 32),
    -- For a dir it changes whenever anything below it changes: sync
    -- clients only descend into subtrees whose ETag moved.
    etag         text NOT NULL DEFAULT replace(gen_random_uuid()::text, '-', ''),
    mtime        timestamptz NOT NULL DEFAULT now(),
    -- Set on the whole deleted subtree; trash_root marks the node the user
    -- actually deleted, which is what the trash lists and restores.
    trashed_at   timestamptz,
    trash_root   boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    -- Only a root is nameless, and a root is always a directory.
    CHECK ((parent_id IS NULL) = (name = '')),
    CHECK (parent_id IS NOT NULL OR kind = 'dir'),
    -- An empty file still has a (zero-size) blob.
    CHECK ((kind = 'file') = (blob_id IS NOT NULL)),
    CHECK (NOT trash_root OR trashed_at IS NOT NULL)
);

CREATE UNIQUE INDEX nodes_root_key ON nodes (owner_id) WHERE parent_id IS NULL;
-- Trashed nodes are exempt: deleting "a.txt" and creating a new "a.txt"
-- must work, and restoring resolves the conflict then.
CREATE UNIQUE INDEX nodes_name_key ON nodes (parent_id, name_key) WHERE trashed_at IS NULL;
-- Listing by name with dirs first ('dir' < 'file') and a keyset cursor
-- (R3). Not partial: it also backs the parent_id foreign key, whose
-- cascade has to find trashed children too.
CREATE INDEX nodes_list_name ON nodes (parent_id, kind, name_key, id);
CREATE INDEX nodes_list_mtime ON nodes (parent_id, mtime, id) WHERE trashed_at IS NULL;
CREATE INDEX nodes_list_size ON nodes (parent_id, size, id) WHERE trashed_at IS NULL;
CREATE INDEX nodes_trash ON nodes (owner_id, trashed_at) WHERE trash_root;
CREATE INDEX nodes_blob ON nodes (blob_id);
-- Substring search by name (R5).
CREATE INDEX nodes_name_trgm ON nodes USING gin (name_key gin_trgm_ops) WHERE trashed_at IS NULL;

CREATE TABLE versions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id    uuid NOT NULL REFERENCES nodes ON DELETE CASCADE,
    blob_id    uuid NOT NULL REFERENCES blobs,
    -- Copied from the blob so the version list needs no join.
    size       bigint NOT NULL CHECK (size >= 0),
    hash       bytea NOT NULL CHECK (length(hash) = 32),
    -- Kept when the author's account is deleted.
    author_id  uuid REFERENCES users ON DELETE SET NULL,
    label      text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX versions_node ON versions (node_id, created_at DESC);
CREATE INDEX versions_blob ON versions (blob_id);

-- +goose Down
DROP TABLE versions;
DROP TABLE nodes;
DROP TABLE blobs;
DROP TABLE users;
DROP COLLATION name_order;
