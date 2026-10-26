-- +goose Up
CREATE TABLE sessions (
    -- SHA-256 of the cookie value. The value itself is never stored, so a
    -- leaked database dump can't be replayed as live sessions.
    id_hash      bytea PRIMARY KEY CHECK (length(id_hash) = 32),
    user_id      uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    -- Shown in "active sessions" and the audit log; never used for auth.
    ip           inet,
    user_agent   text NOT NULL DEFAULT ''
);

-- "Log out everywhere" and disabling a user delete by user.
CREATE INDEX sessions_user ON sessions (user_id);

-- +goose Down
DROP TABLE sessions;
