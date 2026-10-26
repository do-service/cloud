-- +goose Up
-- Groups (R15) and shares (R9). A share grants a user or a group rights on
-- a node and everything below it; the rights a user has on a node are the
-- union of every share on the node's path to them or their groups
-- (architecture §6, internal/access).
CREATE TABLE groups (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       citext NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 100),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id uuid NOT NULL REFERENCES groups ON DELETE CASCADE,
    user_id  uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);

-- "Which groups is this user in", asked on every access check.
CREATE INDEX group_members_user ON group_members (user_id);

CREATE TABLE shares (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id      uuid NOT NULL REFERENCES nodes ON DELETE CASCADE,
    grantee_type text NOT NULL CHECK (grantee_type IN ('user', 'group')),
    -- A user or a group by grantee_type, so no foreign key: the services
    -- that delete users and groups delete their shares too.
    grantee_id   uuid NOT NULL,
    -- Bit mask: 1 read, 2 write, 4 reshare; read is always included.
    perms        smallint NOT NULL CHECK (perms BETWEEN 1 AND 7 AND perms & 1 = 1),
    created_by   uuid REFERENCES users ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (node_id, grantee_type, grantee_id)
);

-- "Shared with me" and the access check's lookup by grantee.
CREATE INDEX shares_grantee ON shares (grantee_type, grantee_id);

-- +goose Down
DROP TABLE shares;
DROP TABLE group_members;
DROP TABLE groups;
