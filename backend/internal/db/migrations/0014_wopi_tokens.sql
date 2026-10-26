-- +goose Up
-- WOPI access tokens (R12): what an online editor presents for one user
-- and one file. Only the SHA-256 of a token is kept; rights are checked
-- on every request, so a revoked share ends editing at once.
CREATE TABLE wopi_tokens (
    hash bytea PRIMARY KEY CHECK (length(hash) = 32),
    user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    node_id uuid NOT NULL REFERENCES nodes ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);
CREATE INDEX wopi_tokens_expires ON wopi_tokens (expires_at);

-- +goose Down
DROP TABLE wopi_tokens;
