-- +goose Up
-- Previews (R6) made so far, by blob and size: the image itself is in
-- storage at previews/{blob_id}/{size}.webp. A failed one is kept too
-- (ok false), so a file that can't be previewed isn't tried on every
-- view. They go with their blob.
CREATE TABLE previews (
    blob_id uuid NOT NULL REFERENCES blobs ON DELETE CASCADE,
    size text NOT NULL,
    ok boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (blob_id, size)
);

-- +goose Down
DROP TABLE previews;
