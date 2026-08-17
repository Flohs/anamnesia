-- 0008_embed_dims_default_and_indexes: repair the default install.
--
-- Two defects shipped in 0004-0006, which flip-flopped the embedding
-- column width for benchmark runs:
--
--  1. The chain ended at vector(2048) while the documented + default
--     embed width stayed 1536 (embed.dims / ANAMNESIA_EMBED_DIMS, and the
--     stub embedder). Every embedding write on a fresh install failed with
--     "expected 2048 dimensions, not 1536".
--
--  2. 0006 dropped the three HNSW indexes (pgvector caps HNSW at 2000
--     dimensions) and no later migration recreated them, so every vector
--     search degraded to a sequential scan permanently.
--
-- This migration returns the columns to the shipped default of 1536 and
-- recreates the indexes. Existing vectors are discarded (they were either
-- unwritable or the wrong width anyway); the embed backfill worker
-- re-embeds on its next tick because embedding becomes NULL.
--
-- Running deliberately at another width is still supported, but it is a
-- runtime decision rather than a migration: set embed.dims and run
-- `anamnesia migrate --dims N`, which re-dimensions and rebuilds indexes.
-- The server refuses to boot when the schema and embed.dims disagree, so
-- this can no longer fail silently.

-- +goose Up
BEGIN;

DROP INDEX IF EXISTS facts_embedding;
DROP INDEX IF EXISTS experiences_embedding;
DROP INDEX IF EXISTS entities_embedding;

ALTER TABLE facts        ALTER COLUMN embedding TYPE vector(1536) USING NULL;
ALTER TABLE experiences  ALTER COLUMN embedding TYPE vector(1536) USING NULL;
ALTER TABLE entities     ALTER COLUMN embedding TYPE vector(1536) USING NULL;

CREATE INDEX facts_embedding       ON facts       USING hnsw(embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
CREATE INDEX experiences_embedding ON experiences USING hnsw(embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
CREATE INDEX entities_embedding    ON entities    USING hnsw(embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

COMMIT;

-- +goose Down
-- Return to the (broken) 0006 state: 2048 wide, no ANN indexes.
BEGIN;

DROP INDEX IF EXISTS facts_embedding;
DROP INDEX IF EXISTS experiences_embedding;
DROP INDEX IF EXISTS entities_embedding;

ALTER TABLE facts        ALTER COLUMN embedding TYPE vector(2048) USING NULL;
ALTER TABLE experiences  ALTER COLUMN embedding TYPE vector(2048) USING NULL;
ALTER TABLE entities     ALTER COLUMN embedding TYPE vector(2048) USING NULL;

COMMIT;
