-- +goose Up
-- When a version stopped being the current content. Its created_at is
-- the content's own mtime, which a sync client now sets (E11-3) and may
-- be years old: pruning by it would drop a version the moment it is made.
-- Retention counts from here instead. Existing rows had server mtimes.
ALTER TABLE versions ADD COLUMN replaced_at timestamptz;
UPDATE versions SET replaced_at = created_at;
ALTER TABLE versions ALTER COLUMN replaced_at SET NOT NULL, ALTER COLUMN replaced_at SET DEFAULT now();

-- +goose Down
ALTER TABLE versions DROP COLUMN replaced_at;
