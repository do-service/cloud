-- +goose Up
-- Search is per user (R5; shares join in E6), so the trigram index leads
-- with owner_id: GIN intersects the owner's entries with the trigrams'
-- inside one index scan, instead of matching every user's names and
-- filtering them by owner afterwards. btree_gin provides uuid in a GIN
-- index; it is a trusted extension, like the ones in 0001.
CREATE EXTENSION IF NOT EXISTS btree_gin;
DROP INDEX nodes_name_trgm;
CREATE INDEX nodes_search ON nodes USING gin (owner_id, name_key gin_trgm_ops) WHERE trashed_at IS NULL;

-- +goose Down
DROP INDEX nodes_search;
CREATE INDEX nodes_name_trgm ON nodes USING gin (name_key gin_trgm_ops) WHERE trashed_at IS NULL;
DROP EXTENSION IF EXISTS btree_gin;
