-- +goose Up
-- Folders come first in every sort order, not only by name (E3-1), so the
-- date and size indexes lead with kind too: a page then reads one range of
-- one index per kind, in either direction, without sorting.
DROP INDEX nodes_list_mtime;
DROP INDEX nodes_list_size;
CREATE INDEX nodes_list_mtime ON nodes (parent_id, kind, mtime, id) WHERE trashed_at IS NULL;
CREATE INDEX nodes_list_size ON nodes (parent_id, kind, size, id) WHERE trashed_at IS NULL;

-- +goose Down
DROP INDEX nodes_list_mtime;
DROP INDEX nodes_list_size;
CREATE INDEX nodes_list_mtime ON nodes (parent_id, mtime, id) WHERE trashed_at IS NULL;
CREATE INDEX nodes_list_size ON nodes (parent_id, size, id) WHERE trashed_at IS NULL;
