-- +goose Up
-- Swap embedding columns from vector(1536) to vector(2048) for the
-- nvidia/llama-nemotron-embed-vl-1b-v2 model on OpenRouter (and other
-- 2048-dim embedders). Existing 1536-dim vectors are discarded; the
-- embed worker backfills on its next tick.
--
-- Note: pgvector caps HNSW (and IVFFlat) at 2000 dimensions for the
-- vector type, so we do NOT recreate the indexes here — retrieval
-- falls back to sequential scan. Fine for single-host benchmark
-- workloads; for production-scale corpora switch to halfvec(2048),
-- which raises the HNSW limit to 4000.

BEGIN;

DROP INDEX IF EXISTS facts_embedding;
DROP INDEX IF EXISTS experiences_embedding;
DROP INDEX IF EXISTS entities_embedding;

ALTER TABLE facts        ALTER COLUMN embedding TYPE vector(2048) USING NULL;
ALTER TABLE experiences  ALTER COLUMN embedding TYPE vector(2048) USING NULL;
ALTER TABLE entities     ALTER COLUMN embedding TYPE vector(2048) USING NULL;

COMMIT;

-- +goose Down
BEGIN;

ALTER TABLE facts        ALTER COLUMN embedding TYPE vector(1536) USING NULL;
ALTER TABLE experiences  ALTER COLUMN embedding TYPE vector(1536) USING NULL;
ALTER TABLE entities     ALTER COLUMN embedding TYPE vector(1536) USING NULL;

CREATE INDEX facts_embedding       ON facts       USING hnsw(embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
CREATE INDEX experiences_embedding ON experiences USING hnsw(embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
CREATE INDEX entities_embedding    ON entities    USING hnsw(embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

COMMIT;
