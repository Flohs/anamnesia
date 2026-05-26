-- 0003_sources_episodes: generic ingest substrate.
-- A `sources` row holds raw content from any stream (chat turns, transcripts,
-- documents). It's not a memory yet — the extractor worker reads pending
-- rows, decides what to keep, and writes facts / experiences. Raw content
-- expires after `expires_at` so we never accumulate the full chat history.
--
-- Experiences gain first-class episode columns so we can answer
-- temporal-window questions ("last week's standups") in SQL.

-- +goose Up

CREATE TABLE sources (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id      UUID REFERENCES projects(id) ON DELETE SET NULL,
    -- Free-form kind hint: chat-turn | transcript | document | note |
    -- tool-output | email | calendar | …. The extractor doesn't branch on
    -- kind today (LLM reads text either way), but the column is here so
    -- queries like "facts that came from chat" stay cheap.
    kind            TEXT NOT NULL,
    external_ref    TEXT,
    title           TEXT,
    participants    TEXT[] NOT NULL DEFAULT '{}',
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    ingested_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    raw_content     TEXT,
    metadata        JSONB,
    -- Extraction state machine: pending → done | failed | skipped.
    -- Skipped means the surprise/importance gate said "nothing to do".
    extraction_state TEXT NOT NULL DEFAULT 'pending'
                    CHECK (extraction_state IN ('pending','done','failed','skipped')),
    extracted_at    TIMESTAMPTZ,
    extraction_error TEXT,
    -- Operations count produced by the last extractor run, for observability.
    ops_produced    INT NOT NULL DEFAULT 0,
    -- When set, the worker keeps raw_content forever (e.g. user explicitly
    -- wants to preserve a transcript). Default is FALSE — raw_content gets
    -- nulled out after expires_at.
    preserve_raw    BOOLEAN NOT NULL DEFAULT FALSE,
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '7 days')
);

CREATE INDEX sources_user_project_occurred
    ON sources (user_id, project_id, occurred_at DESC);
CREATE INDEX sources_pending
    ON sources (ingested_at)
    WHERE extraction_state = 'pending';
CREATE INDEX sources_expiry
    ON sources (expires_at)
    WHERE raw_content IS NOT NULL AND preserve_raw = FALSE;

-- ─── facts: link to source of origin ───────────────────────────────────
ALTER TABLE facts ADD COLUMN source_id UUID
    REFERENCES sources(id) ON DELETE SET NULL;
CREATE INDEX facts_source ON facts(source_id) WHERE source_id IS NOT NULL;

-- ─── experiences: promote to first-class episodes ──────────────────────
ALTER TABLE experiences
    ADD COLUMN source_id     UUID REFERENCES sources(id) ON DELETE SET NULL,
    ADD COLUMN occurred_at   TIMESTAMPTZ,
    ADD COLUMN participants  TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN topic         TEXT,
    ADD COLUMN parent_id     UUID REFERENCES experiences(id) ON DELETE SET NULL,
    ADD COLUMN provenance    JSONB;

-- BRIN is cheap and perfect for time-window scans on append-mostly tables.
CREATE INDEX experiences_occurred_at      ON experiences USING brin(occurred_at);
CREATE INDEX experiences_topic            ON experiences (topic)
    WHERE topic IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX experiences_participants     ON experiences USING gin(participants);
CREATE INDEX experiences_parent           ON experiences (parent_id)
    WHERE parent_id IS NOT NULL;
CREATE INDEX experiences_source           ON experiences (source_id)
    WHERE source_id IS NOT NULL;

-- Backfill occurred_at so the column is non-nullable in practice. We don't
-- enforce NOT NULL because the column is also written by paths that don't
-- carry a source (e.g. the existing session-end fold).
UPDATE experiences SET occurred_at = ingested_at WHERE occurred_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS experiences_source;
DROP INDEX IF EXISTS experiences_parent;
DROP INDEX IF EXISTS experiences_participants;
DROP INDEX IF EXISTS experiences_topic;
DROP INDEX IF EXISTS experiences_occurred_at;
ALTER TABLE experiences
    DROP COLUMN IF EXISTS provenance,
    DROP COLUMN IF EXISTS parent_id,
    DROP COLUMN IF EXISTS topic,
    DROP COLUMN IF EXISTS participants,
    DROP COLUMN IF EXISTS occurred_at,
    DROP COLUMN IF EXISTS source_id;

DROP INDEX IF EXISTS facts_source;
ALTER TABLE facts DROP COLUMN IF EXISTS source_id;

DROP INDEX IF EXISTS sources_expiry;
DROP INDEX IF EXISTS sources_pending;
DROP INDEX IF EXISTS sources_user_project_occurred;
DROP TABLE IF EXISTS sources;
