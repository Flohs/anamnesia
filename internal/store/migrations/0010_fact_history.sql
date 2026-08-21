-- 0010_fact_history: let a fact keep its previous values.
--
-- 0001 made the identity index cover every non-deleted row, so there
-- could only ever be one row per (user, project, fact_scope, key) and
-- UpsertFact had no choice but to overwrite. The table has carried
-- valid_to, invalidated_at and superseded_by since 0001 and nothing has
-- ever written them.
--
-- Narrowing the predicate to current rows only is what lets versions
-- accumulate: a superseded row is exempt from the constraint, so the
-- replacement can be inserted alongside it. Exactly one *current* row
-- per key is still guaranteed, which is the invariant every reader
-- depends on.
--
-- Every existing row has superseded_by IS NULL, so on an install with no
-- history the new index covers precisely the same set as the old one.
-- This is an index rebuild, not a data change.

-- +goose Up
DROP INDEX IF EXISTS facts_identity;

CREATE UNIQUE INDEX facts_identity
    ON facts (user_id, coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid), fact_scope, key)
    WHERE deleted_at IS NULL AND superseded_by IS NULL;

-- Walking a key's history, and the "current rows only" filter every
-- reader now carries.
CREATE INDEX facts_superseded_by ON facts (superseded_by) WHERE superseded_by IS NOT NULL;

-- +goose Down
-- Fails if any key has history: the old index permits one row per key
-- full stop, and there is no correct way to choose which version to
-- discard. Delete the superseded rows deliberately before rolling back.
DROP INDEX IF EXISTS facts_superseded_by;
DROP INDEX IF EXISTS facts_identity;

CREATE UNIQUE INDEX facts_identity
    ON facts (user_id, coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid), fact_scope, key)
    WHERE deleted_at IS NULL;
