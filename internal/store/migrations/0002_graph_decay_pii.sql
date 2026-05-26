-- 0002_graph_decay_pii: graph (entities + edges), decay relevance column,
-- PII tags column on facts/experiences.

-- +goose Up

-- ─── Decay: relevance column on experiences ────────────────────────────
-- experiences.relevance is recomputed by the decay worker as a function
-- of (importance, time, use_count). Default 0.5 so newly-ingested rows
-- aren't disadvantaged before the first decay tick runs.
ALTER TABLE experiences ADD COLUMN relevance REAL NOT NULL DEFAULT 0.5;

CREATE INDEX experiences_relevance ON experiences (relevance DESC) WHERE deleted_at IS NULL;

-- ─── PII tags: array of detected entity types per row ──────────────────
-- Empty array (default) means "PII detector ran and found nothing" or
-- "detector is configured to none". NULL would have been confusing —
-- explicit empty array reads cleanly in retrieval filters.
ALTER TABLE facts       ADD COLUMN pii_tags TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE experiences ADD COLUMN pii_tags TEXT[] NOT NULL DEFAULT '{}';

-- ─── Graph: entities ───────────────────────────────────────────────────
-- A node in the memory graph. Identity = (user_id, project_id-or-zero,
-- kind, name). project_id NULL = entity lives at the user level.
CREATE TABLE entities (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    project_id  UUID          REFERENCES projects(id) ON DELETE SET NULL,
    kind        TEXT NOT NULL,
    name        TEXT NOT NULL,
    props       JSONB NOT NULL DEFAULT '{}',
    embedding   vector(1536),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX entities_identity
    ON entities (user_id, coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid), kind, name);
CREATE INDEX entities_user_project ON entities (user_id, project_id);
CREATE INDEX entities_embedding    ON entities USING hnsw(embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

-- ─── Graph: edges ──────────────────────────────────────────────────────
-- Typed, bitemporal relations between two entities. Edges inherit
-- ownership from from_id (the cascading delete on entities sweeps any
-- edges that pointed at them).
CREATE TABLE edges (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    from_id         UUID NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_id           UUID NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL,
    props           JSONB NOT NULL DEFAULT '{}',

    valid_from      TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to        TIMESTAMPTZ,
    ingested_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    invalidated_at  TIMESTAMPTZ,

    source          TEXT NOT NULL DEFAULT 'system',
    trust           REAL NOT NULL DEFAULT 0.5
);

CREATE INDEX edges_from_kind_valid ON edges (from_id, kind, valid_from, valid_to);
CREATE INDEX edges_to_kind_valid   ON edges (to_id,   kind, valid_from, valid_to);
CREATE INDEX edges_kind            ON edges (kind);

-- +goose Down
DROP TABLE IF EXISTS edges;
DROP TABLE IF EXISTS entities;
ALTER TABLE experiences DROP COLUMN IF EXISTS pii_tags;
ALTER TABLE facts       DROP COLUMN IF EXISTS pii_tags;
DROP INDEX IF EXISTS experiences_relevance;
ALTER TABLE experiences DROP COLUMN IF EXISTS relevance;
