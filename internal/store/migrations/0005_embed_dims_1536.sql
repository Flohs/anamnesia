-- +goose Up
-- Swap embedding columns back from vector(768) (Ollama nomic-embed-text)
-- to vector(1536) (OpenAI text-embedding-3-small / text-embedding-3-large
-- default). Existing 768-dim vectors are discarded; the embed worker
-- backfills on its next tick.

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
