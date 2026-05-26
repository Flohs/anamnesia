-- 0001_init: single-tenant Anamnesia schema. Single owner, optional
-- multiple users (small team). No tenants, no row-level security — the
-- whole DB is one trust boundary.

-- +goose Up
-- +goose StatementBegin
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS vector;
-- +goose StatementEnd

-- users: optional handle table for small teams.
CREATE TABLE users (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    handle      TEXT NOT NULL UNIQUE,
    display     TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- projects: a slug per repository / working directory. NULL ProjectID on
-- a memory row means "lives at the user level" (e.g. persona facts).
CREATE TABLE projects (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slug        TEXT NOT NULL,
    display     TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, slug)
);

-- facts: keyed claims. Identity = (user_id, project_id, scope, key).
-- project_id is NULL for user/environment-scoped facts.
CREATE TABLE facts (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id      UUID REFERENCES projects(id) ON DELETE SET NULL,
    fact_scope      TEXT NOT NULL CHECK (fact_scope IN ('user','project','environment')),
    key             TEXT NOT NULL,
    value           JSONB NOT NULL,
    source          TEXT,
    trust           REAL NOT NULL DEFAULT 0.5,
    embedding       vector(1536),
    embed_model     TEXT,
    valid_from      TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to        TIMESTAMPTZ,
    ingested_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    invalidated_at  TIMESTAMPTZ,
    superseded_by   UUID,
    deleted_at      TIMESTAMPTZ,
    tsv             tsvector GENERATED ALWAYS AS (
        to_tsvector('english', coalesce(key,'') || ' ' || coalesce(value::text,''))
    ) STORED
);

CREATE UNIQUE INDEX facts_identity
    ON facts (user_id, coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid), fact_scope, key)
    WHERE deleted_at IS NULL;
CREATE INDEX facts_user_project ON facts (user_id, project_id) WHERE deleted_at IS NULL;
CREATE INDEX facts_tsv          ON facts USING gin(tsv)         WHERE deleted_at IS NULL;
CREATE INDEX facts_embedding    ON facts USING hnsw(embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

-- experiences: trajectories / strategies / insights, append-only via
-- superseded_by chains.
CREATE TABLE experiences (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id      UUID REFERENCES projects(id) ON DELETE SET NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('case','strategy','hybrid')),
    abstraction     INT NOT NULL DEFAULT 0,
    title           TEXT,
    body            TEXT NOT NULL,
    outcome         TEXT,
    meta            JSONB,
    trust           REAL NOT NULL DEFAULT 0.5,
    importance      REAL NOT NULL DEFAULT 0.5,
    use_count       INT NOT NULL DEFAULT 0,
    last_used_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    embedding       vector(1536),
    embed_model     TEXT,
    valid_from      TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to        TIMESTAMPTZ,
    ingested_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    invalidated_at  TIMESTAMPTZ,
    superseded_by   UUID,
    deleted_at      TIMESTAMPTZ,
    tsv             tsvector GENERATED ALWAYS AS (
        to_tsvector('english', coalesce(title,'') || ' ' || coalesce(body,''))
    ) STORED
);

CREATE INDEX experiences_user_project ON experiences (user_id, project_id) WHERE deleted_at IS NULL;
CREATE INDEX experiences_tsv          ON experiences USING gin(tsv)         WHERE deleted_at IS NULL;
CREATE INDEX experiences_embedding    ON experiences USING hnsw(embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

-- skills: callables (functions, scripts, APIs, remote MCP). Identity =
-- (user_id, project_id, name). project_id NULL = user-level skill.
CREATE TABLE skills (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id      UUID REFERENCES projects(id) ON DELETE SET NULL,
    name            TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('function','script','api','mcp')),
    description     TEXT,
    signature       JSONB,
    body            TEXT,
    meta            JSONB,
    use_count       INT NOT NULL DEFAULT 0,
    last_used_at    TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX skills_identity
    ON skills (user_id, coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid), name)
    WHERE deleted_at IS NULL;
CREATE INDEX skills_user_project ON skills (user_id, project_id) WHERE deleted_at IS NULL;

-- working_memory: in-session entries, with TTL. Position is the order
-- within (user_id, session_id).
CREATE TABLE working_memory (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id      UUID REFERENCES projects(id) ON DELETE SET NULL,
    session_id      UUID NOT NULL,
    position        INT NOT NULL,
    role            TEXT NOT NULL CHECK (role IN ('observation','plan','state','tool_output')),
    body            TEXT NOT NULL,
    meta            JSONB,
    folded_into     UUID REFERENCES experiences(id) ON DELETE SET NULL,
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '24 hours'),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, session_id, position)
);

CREATE INDEX working_memory_session ON working_memory (user_id, session_id);
CREATE INDEX working_memory_expires ON working_memory (expires_at) WHERE folded_into IS NULL;

-- audit_log: every meaningful read/write. Append-only.
CREATE TABLE audit_log (
    id          BIGSERIAL PRIMARY KEY,
    at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_id     UUID,
    project_id  UUID,
    op          TEXT NOT NULL,
    target      TEXT NOT NULL,
    target_id   UUID,
    actor       TEXT NOT NULL DEFAULT 'system',
    payload     JSONB
);

CREATE INDEX audit_log_user_project ON audit_log (user_id, project_id, at DESC);
CREATE INDEX audit_log_at           ON audit_log (at DESC);

-- jobs: background queue (embed backfill, consolidation, forget).
CREATE TABLE jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind            TEXT NOT NULL,
    payload         JSONB NOT NULL,
    state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','running','done','failed')),
    attempts        INT NOT NULL DEFAULT 0,
    scheduled_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    error           TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX jobs_pending ON jobs (scheduled_at) WHERE state = 'pending';
CREATE INDEX jobs_state   ON jobs (state);

-- +goose Down
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS working_memory;
DROP TABLE IF EXISTS skills;
DROP TABLE IF EXISTS experiences;
DROP TABLE IF EXISTS facts;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS users;
