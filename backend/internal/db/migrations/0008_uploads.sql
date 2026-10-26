-- +goose Up
-- Uploads in progress (R2): what a finished upload becomes, and the bytes
-- it holds against the quota until then (R16). A row is deleted when the
-- upload is finalized into a node or abandoned.
CREATE TABLE uploads (
    id          uuid PRIMARY KEY,
    -- Who uploads, and whose quota it counts against: the same person
    -- until shares let one user upload into another's folder (E6).
    user_id     uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    -- Checked again at finalization: the folder may be gone by then.
    parent_id   uuid NOT NULL,
    name        text NOT NULL,
    size        bigint NOT NULL CHECK (size >= 0),
    -- The client's SHA-256, verified at finalization when given.
    sha256      bytea CHECK (length(sha256) = 32),
    on_conflict text NOT NULL CHECK (on_conflict IN ('fail', 'rename')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- Past it the reservation no longer counts and the cleanup job (E3-5)
    -- removes the row and its data.
    expires_at  timestamptz NOT NULL
);

CREATE INDEX uploads_user ON uploads (user_id, expires_at);

-- +goose Down
DROP TABLE uploads;
