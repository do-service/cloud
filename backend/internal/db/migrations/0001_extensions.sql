-- +goose Up
-- pgcrypto: gen_random_uuid() for primary keys; citext: case-insensitive
-- usernames/emails; pg_trgm: trigram indexes for filename search. All three
-- are trusted extensions (PG13+), so the database owner can create them
-- without superuser.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- +goose Down
DROP EXTENSION IF EXISTS pg_trgm;
DROP EXTENSION IF EXISTS citext;
DROP EXTENSION IF EXISTS pgcrypto;
