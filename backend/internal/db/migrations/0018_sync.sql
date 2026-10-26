-- +goose Up
-- Change journal for sync clients (E11, docs/desktop-client.md). Every
-- write to a user's tree adds a row in the same transaction: "node X
-- changed at seq N", a dirty set rather than events — the sync API reads
-- the node's current state. A moved, trashed or restored folder gets one
-- row, not one per node below it.
CREATE TABLE sync_heads (
    user_id    uuid PRIMARY KEY REFERENCES users ON DELETE CASCADE,
    -- The last seq in the journal of their tree.
    tree_seq   bigint NOT NULL DEFAULT 0,
    -- Everything up to min_seq is deleted: an older cursor can't resume.
    min_seq    bigint NOT NULL DEFAULT 0,
    -- Changes when what they see in others' trees does (shares, groups).
    access_seq bigint NOT NULL DEFAULT 0
);

CREATE TABLE sync_journal (
    owner_id  uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    seq       bigint NOT NULL,
    -- No foreign key: the row outlives a purge, it is the tombstone.
    node_id   uuid NOT NULL,
    -- A hint only (fs journalOp): create, content, move, trash, restore.
    op        smallint NOT NULL,
    -- Shared nodes on the node's old and new chain, NULL for none: whose
    -- recipients the change may concern.
    scopes    uuid[],
    -- The device that made it, so it can skip its own changes.
    device_id uuid,
    at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, seq)
);

-- Retention deletes by age (SYNC_JOURNAL_RETENTION).
CREATE INDEX sync_journal_at ON sync_journal (at);

-- +goose Down
DROP TABLE sync_journal;
DROP TABLE sync_heads;
