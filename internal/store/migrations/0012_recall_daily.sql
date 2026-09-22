-- 0012_recall_daily: how often retrieval actually recalled something.
--
-- The console could report how many memories exist and how many sources
-- failed, but nothing said whether any of it ever came back when a prompt
-- asked for it. The activity recorder holds retrievals in memory and a
-- restart empties it, so the question "how has this been doing" had no
-- answer beyond whatever happened since the server came up.
--
-- Counters rather than one row per retrieval. A row per prompt would buy
-- a per-prompt drill-down nothing asks for yet, and would have to be
-- pruned; three integers a day are small enough to keep forever, which is
-- what makes a retrospective possible at all.
--
-- Deliberately not project-scoped. A project_id column would put this
-- table into projectScopedTables, and `project prune` treats a row in any
-- of those as "this project still holds something" — so a project you
-- opened once and never stored memory in would stop being prunable
-- because of its own counters. A project holding nothing but a tally of
-- its own emptiness is exactly what prune exists to remove.
--
-- prompts counts every graded retrieval. recalled is how many returned a
-- hit clearing the absolute bar, and ungraded how many had no absolute
-- number to judge by (no embedder configured, or nothing came back from
-- the vector channel). Misses are the remainder, and are not stored
-- separately: a derived number cannot disagree with the ones it is
-- derived from.

-- +goose Up
CREATE TABLE recall_daily (
    user_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day      DATE NOT NULL,
    prompts  INTEGER NOT NULL DEFAULT 0,
    recalled INTEGER NOT NULL DEFAULT 0,
    ungraded INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, day)
);

-- +goose Down
DROP TABLE IF EXISTS recall_daily;
