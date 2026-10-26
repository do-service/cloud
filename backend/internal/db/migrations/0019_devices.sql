-- +goose Up
-- Desktop sync clients signed in through the browser (E12,
-- docs/desktop-client.md). Each holds a bearer token `cld_…` of its own,
-- so one laptop can be signed out without touching the others, and its
-- changes are told apart in sync_journal.device_id (no foreign key there:
-- a journal row outlives its device, it only says who made the change).
CREATE TABLE devices (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    name           text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    platform       text NOT NULL CHECK (platform IN ('macos', 'windows', 'linux')),
    client_version text NOT NULL CHECK (length(client_version) <= 64),
    -- SHA-256 of the token, as for app passwords: 256 random bits need no
    -- slow hash, and the client sends the token with every request.
    token_hash     bytea NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- Both written at most once an hour (auth.deviceUsedEvery).
    last_used_at   timestamptz,
    last_ip        inet
);

CREATE INDEX devices_user ON devices (user_id);

-- +goose Down
DROP TABLE devices;
