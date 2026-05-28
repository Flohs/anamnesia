-- +goose Up
-- Swap embedding columns from vector(1536) (text-embedding-3-small) to
-- vector(768) for Ollama's nomic-embed-text. HNSW indexes depend on the
-- column type so we drop them first, alter, then recreate.
--
-- This is a lossy migration: existing 1536-dim vectors are discarded.
-- The embed worker will backfill on its next tick once the embed model
-- is configured correctly. If you want to keep existing data, switch
-- back to a 1536-dim model before running this.

BEGIN;

DROP INDEX IF EXISTS facts_embedding;
DROP INDEX IF EXISTS experiences_embedding;
DROP INDEX IF EXISTS entities_embedding;

ALTER TABLE facts        ALTER COLUMN embedding TYPE vector(768) USING NULL;
ALTER TABLE experiences  ALTER COLUMN embedding TYPE vector(768) USING NULL;
ALTER TABLE entities     ALTER COLUMN embedding TYPE vector(768) USING NULL;

CREATE INDEX facts_embedding       ON facts       USING hnsw(embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
CREATE INDEX experiences_embedding ON experiences USING hnsw(embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
CREATE INDEX entities_embedding    ON entities    USING hnsw(embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

COMMIT;

-- +goose Down
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
