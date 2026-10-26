-- +goose Up
-- An upload may now replace a file of the same name with a new version of
-- it (E4-1).
ALTER TABLE uploads DROP CONSTRAINT uploads_on_conflict_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_on_conflict_check CHECK (on_conflict IN ('fail', 'rename', 'replace'));

-- +goose Down
DELETE FROM uploads WHERE on_conflict = 'replace';
ALTER TABLE uploads DROP CONSTRAINT uploads_on_conflict_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_on_conflict_check CHECK (on_conflict IN ('fail', 'rename'));
