-- Sync paused by the user (cloudsync pause, the app's «Пауза»): kept in
-- the database so a running engine of another process sees it, and a
-- restart doesn't resume behind the user's back.
ALTER TABLE account ADD COLUMN paused INTEGER NOT NULL DEFAULT 0;
