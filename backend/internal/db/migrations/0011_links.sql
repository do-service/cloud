-- +goose Up
-- Public links (R10): anyone with the URL gets the link's rights on its
-- node's subtree, by mode: view or download (read), upload (write only, a
-- file drop).
CREATE TABLE links (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id       uuid NOT NULL REFERENCES nodes ON DELETE CASCADE,
    -- SHA-256 of the token in the URL: what a request is looked up by.
    token_hash    bytea NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    -- The token itself, sealed with SECRET_KEY, so its owner can copy
    -- the link again; a database without the key doesn't give it away.
    token_enc     bytea NOT NULL,
    mode          text NOT NULL CHECK (mode IN ('view', 'download', 'upload')),
    -- Argon2id, NULL for no password.
    password_hash text,
    expires_at    timestamptz,
    created_by    uuid REFERENCES users ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX links_node ON links (node_id);

-- A browser that gave a link's password: like sessions, the cookie value
-- is never stored, only its SHA-256.
CREATE TABLE link_sessions (
    id_hash    bytea PRIMARY KEY CHECK (length(id_hash) = 32),
    link_id    uuid NOT NULL REFERENCES links ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);

-- An upload through a link belongs to the folder owner's quota (user_id)
-- and can be continued only through that link.
ALTER TABLE uploads ADD COLUMN link_id uuid REFERENCES links ON DELETE CASCADE;

-- +goose Down
ALTER TABLE uploads DROP COLUMN link_id;
DROP TABLE link_sessions;
DROP TABLE links;
