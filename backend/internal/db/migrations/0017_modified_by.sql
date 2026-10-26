-- +goose Up
-- Who wrote a file's current content: when it's replaced, this becomes the
-- author of the version that keeps it (R7). Files from before stay
-- unknown rather than guessed.
ALTER TABLE nodes ADD COLUMN modified_by uuid REFERENCES users ON DELETE SET NULL;

-- +goose Down
ALTER TABLE nodes DROP COLUMN modified_by;
