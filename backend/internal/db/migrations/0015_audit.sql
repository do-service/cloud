-- +goose Up
-- Audit log (R19): who did what to what, from where. No foreign keys: an
-- entry outlives its user and its node, so it keeps their names.
CREATE TABLE audit_log (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at timestamptz NOT NULL DEFAULT now(),
    actor_id uuid,
    actor_name text NOT NULL DEFAULT '',
    ip inet,
    action text NOT NULL,
    target_id text NOT NULL DEFAULT '',
    target_name text NOT NULL DEFAULT '',
    details jsonb NOT NULL DEFAULT '{}'
);
-- Newest first by id; the others for the admin's filters and retention.
CREATE INDEX audit_log_actor ON audit_log (actor_id, id);
CREATE INDEX audit_log_action ON audit_log (action, id);
CREATE INDEX audit_log_at ON audit_log (at);

-- A user being deleted (E9-11): signed out and kept out like a disabled
-- one while a background job removes their files.
ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check CHECK (status IN ('active', 'disabled', 'deleting'));

-- +goose Down
ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check CHECK (status IN ('active', 'disabled'));
DROP TABLE audit_log;
