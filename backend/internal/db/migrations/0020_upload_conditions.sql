-- +goose Up
-- Conditional uploads for the sync client (E11-3): an upload may replace a
-- file picked by id rather than by name, only if the file still has the
-- ETag the client based its change on, and carry the local mtime. Kept on
-- the row because the conditions are checked again when the upload
-- finishes, hours after it was created.
ALTER TABLE uploads
    -- No foreign key, as for parent_id: finishing checks the file is
    -- still there, and a purged one just fails the upload.
    ADD COLUMN node_id  uuid,
    ADD COLUMN if_match text CHECK (node_id IS NOT NULL OR if_match IS NULL),
    ADD COLUMN mtime    timestamptz;

-- +goose Down
DELETE FROM uploads WHERE node_id IS NOT NULL;
ALTER TABLE uploads DROP COLUMN node_id, DROP COLUMN if_match, DROP COLUMN mtime;
