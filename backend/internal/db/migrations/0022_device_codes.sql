-- +goose Up
-- One-time codes of the desktop sign-in (E12-2, RFC 8252 + PKCE): the
-- consent page issues one for the signed-in user, the client trades it
-- with its code_verifier for a device token. Only the SHA-256 of the code
-- is kept, as for tokens; a row lives two minutes and is deleted on use.
CREATE TABLE device_codes (
    code_hash      bytea PRIMARY KEY CHECK (length(code_hash) = 32),
    user_id        uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    name           text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    platform       text NOT NULL CHECK (platform IN ('macos', 'windows', 'linux')),
    client_version text NOT NULL CHECK (length(client_version) <= 64),
    code_challenge text NOT NULL,
    redirect_uri   text NOT NULL,
    expires_at     timestamptz NOT NULL
);

-- Expired rows are swept on every exchange.
CREATE INDEX device_codes_expires ON device_codes (expires_at);

-- +goose Down
DROP TABLE device_codes;
