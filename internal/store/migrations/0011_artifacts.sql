-- 0011_artifacts: a registry of the pages Claude Code has published.
--
-- Publishing an artifact produces a URL and nothing else keeps it. The
-- URL is in the transcript, so it is not lost, but nothing ever went back
-- for it: measured on one install, 31 artifacts existed across 9 projects
-- and memory held none of them.
--
-- They are their own table rather than facts or experiences, because both
-- of those do something wrong to a durable pointer. Facts are listed
-- wholesale into every session up to a 50 row budget, so artifacts would
-- crowd out the project configuration that block exists for and arrive
-- unbidden instead of being retrieved. Experiences decay and are
-- consolidated, and a consolidation pass that merged two artifacts would
-- take the one field that has to survive verbatim with it.
--
-- Identity is the artifact's own UUID, taken from the URL. Republishing a
-- file redeploys to the same URL, so that is an upsert onto the same row,
-- not a second entry.

-- +goose Up
CREATE TABLE artifacts (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- The project the artifact was first published from. A republish
    -- elsewhere does not move it: it belongs to the work that made it.
    project_id    UUID REFERENCES projects(id) ON DELETE SET NULL,
    artifact_uuid UUID NOT NULL,
    url           TEXT NOT NULL,
    title         TEXT,
    description   TEXT,
    -- Where the source file was at publish time. Usually a session
    -- scratchpad, so it is a record of provenance, not somewhere to look.
    file_path     TEXT,
    -- The readable text of the page, stripped of markup. Empty when it
    -- could not be read, which is the normal case for anything recovered
    -- from a transcript after the scratchpad was cleaned up.
    body          TEXT,
    meta          JSONB,
    embed_model   TEXT,
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ,
    tsv           tsvector GENERATED ALWAYS AS (
        to_tsvector('english',
            coalesce(title,'') || ' ' || coalesce(description,'') || ' ' || coalesce(body,''))
    ) STORED
);

CREATE UNIQUE INDEX artifacts_identity     ON artifacts (user_id, artifact_uuid) WHERE deleted_at IS NULL;
CREATE INDEX        artifacts_user_project ON artifacts (user_id, project_id)    WHERE deleted_at IS NULL;
CREATE INDEX        artifacts_recent       ON artifacts (user_id, occurred_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX        artifacts_tsv          ON artifacts USING gin(tsv)           WHERE deleted_at IS NULL;

-- The embedding column takes whatever width the schema already uses,
-- rather than a literal. Every embedding column has to agree, so an
-- install running at 3072 would otherwise get a 1536 column here and the
-- next `serve` would refuse to boot after an upgrade the user did not ask
-- for. pgvector keeps the dimension in atttypmod directly.
-- +goose StatementBegin
DO $$
DECLARE dims int;
BEGIN
    SELECT a.atttypmod INTO dims
      FROM pg_attribute a
      JOIN pg_class c ON c.oid = a.attrelid
     WHERE c.relname = 'facts' AND a.attname = 'embedding' AND a.attnum > 0;

    EXECUTE format('ALTER TABLE artifacts ADD COLUMN embedding vector(%s)', dims);

    -- pgvector rejects HNSW above 2000 dimensions. Above it the column
    -- still works by sequential scan, which is what every other embedding
    -- table does at that width.
    IF dims <= 2000 THEN
        EXECUTE 'CREATE INDEX artifacts_embedding ON artifacts '
             || 'USING hnsw(embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64)';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS artifacts;
