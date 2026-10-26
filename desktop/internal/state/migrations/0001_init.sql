-- The sync client's state (docs/desktop-client.md, «Клиент», «Движок»).
-- One database per connection (Path(id)), outside the sync folder.
-- STRICT tables: a wrong type from a bug fails the write instead of
-- being stored silently.
-- Times are integers: *_ns in Unix nanoseconds (file mtimes need them),
-- the rest in Unix milliseconds.

-- Exactly one row: who this database syncs, and where (a copy of its
-- registry entry, so the file describes itself).
CREATE TABLE account (
  id          INTEGER PRIMARY KEY CHECK (id = 1),
  server      TEXT NOT NULL,
  user_id     TEXT NOT NULL,
  device_id   TEXT NOT NULL,
  -- The /sync/changes cursor; '' before the first full listing.
  cursor      TEXT NOT NULL DEFAULT '',
  local_root  TEXT NOT NULL,
  -- The last watcher event id handled (FSEvents replays after it).
  watch_since INTEGER NOT NULL DEFAULT 0
) STRICT;

-- Where sync starts on the server, as /sync/roots lists it.
CREATE TABLE roots (
  node_id    TEXT PRIMARY KEY,
  kind       TEXT NOT NULL CHECK (kind IN ('own', 'shared')),
  name       TEXT NOT NULL,
  owner_id   TEXT NOT NULL,
  owner_name TEXT NOT NULL,
  perms      INTEGER NOT NULL,
  etag       TEXT NOT NULL
) STRICT;

-- Selective sync: the nearest rule up a node's chain applies; a root
-- without one is excluded.
CREATE TABLE rules (
  node_id TEXT PRIMARY KEY,
  mode    TEXT NOT NULL CHECK (mode IN ('include', 'exclude'))
) STRICT;

-- The mirror of the server's trees, from /sync/tree and /sync/changes.
-- No foreign key on parent_id: changes may bring a child before its
-- parent, and a shared top has a parent the client never sees.
CREATE TABLE remote (
  node_id   TEXT PRIMARY KEY,
  parent_id TEXT,
  name      TEXT NOT NULL,
  -- The server's name_key (lower-cased NFC), to find a child by name the
  -- way the server compares names.
  name_key  TEXT NOT NULL,
  kind      TEXT NOT NULL CHECK (kind IN ('dir', 'file')),
  size      INTEGER NOT NULL,
  etag      TEXT NOT NULL,
  sha256    TEXT NOT NULL DEFAULT '',
  mtime_ns  INTEGER NOT NULL,
  perms     INTEGER NOT NULL
) STRICT;
CREATE INDEX remote_parent ON remote (parent_id, name_key);

-- The last scan of the sync folder. sha256_cached is valid while size,
-- mtime_ns and file_id are unchanged.
CREATE TABLE local (
  rel_path      TEXT PRIMARY KEY,
  kind          TEXT NOT NULL CHECK (kind IN ('dir', 'file')),
  size          INTEGER NOT NULL,
  mtime_ns      INTEGER NOT NULL,
  file_id       TEXT NOT NULL,
  sha256_cached TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX local_file_id ON local (file_id);

-- What both sides last agreed on: the third state of the three-way
-- reconciliation. Size is kept with mtime and file id so "unchanged
-- locally" is a stat comparison, without hashing.
CREATE TABLE base (
  node_id  TEXT PRIMARY KEY,
  rel_path TEXT NOT NULL UNIQUE,
  kind     TEXT NOT NULL CHECK (kind IN ('dir', 'file')),
  sha256   TEXT NOT NULL DEFAULT '',
  etag     TEXT NOT NULL,
  file_id  TEXT NOT NULL,
  size     INTEGER NOT NULL,
  mtime_ns INTEGER NOT NULL
) STRICT;
CREATE INDEX base_file_id ON base (file_id);

-- The journal of intended actions: an op is written before its action
-- and deleted in the same transaction that records the action's result
-- in base, so after a crash whatever is left here may or may not have
-- happened and is redone idempotently.
CREATE TABLE ops (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  kind       TEXT NOT NULL,
  node_id    TEXT NOT NULL DEFAULT '',
  path       TEXT NOT NULL DEFAULT '',
  params     TEXT NOT NULL DEFAULT '{}',
  state      TEXT NOT NULL CHECK (state IN ('pending', 'running', 'failed')),
  attempts   INTEGER NOT NULL DEFAULT 0,
  next_try   INTEGER NOT NULL DEFAULT 0,
  error_code TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX ops_due ON ops (state, next_try);

-- Resumable transfers of an op: the tus upload URL and how far it got, or
-- the .part file of a download. Gone with the op.
CREATE TABLE transfers (
  op_id     INTEGER PRIMARY KEY REFERENCES ops (id) ON DELETE CASCADE,
  tus_url   TEXT NOT NULL DEFAULT '',
  done      INTEGER NOT NULL DEFAULT 0,
  part_path TEXT NOT NULL DEFAULT ''
) STRICT;

-- What sync can't do on its own and shows in «Проблемы и конфликты».
-- One row per (kind, path, node): detecting the same problem again
-- refreshes it.
CREATE TABLE problems (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  kind       TEXT NOT NULL,
  rel_path   TEXT NOT NULL DEFAULT '',
  node_id    TEXT NOT NULL DEFAULT '',
  code       TEXT NOT NULL DEFAULT '',
  detail     TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE (kind, rel_path, node_id)
) STRICT;

-- «Активность»: the latest ActivityLimit entries, older ones trimmed on
-- insert.
CREATE TABLE activity (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  at       INTEGER NOT NULL,
  kind     TEXT NOT NULL,
  rel_path TEXT NOT NULL,
  detail   TEXT NOT NULL DEFAULT ''
) STRICT;
