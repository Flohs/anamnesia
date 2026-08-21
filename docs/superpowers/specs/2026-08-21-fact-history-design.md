# Design: give facts a history

Written 2026-08-21 against `7635508`.

## The problem

Facts are updated destructively. `UpsertFact` is
`INSERT … ON CONFLICT (user_id, project_id, fact_scope, key) DO UPDATE
SET value = EXCLUDED.value, …`, so there is one row per key forever and
the previous value is overwritten and unrecoverable.

The `facts` table already carries the whole bitemporal apparatus:
`valid_to`, `invalidated_at`, `superseded_by`. **Nothing writes any of
them.** The only write to `superseded_by` anywhere is
`internal/store/experiences.go:101`, for experiences. So facts have the
schema for history and none of the behaviour.

Three consequences:

- There is no fact history. "What did I believe last month" is
  unanswerable, and `anamnesia_experience_supersede` has no fact
  counterpart.
- Every temporal predicate over facts is vacuous, because `valid_to` and
  `invalidated_at` are permanently NULL.
- `knowledge-update`, an entire LongMemEval category, is answered
  correctly only incidentally: you get the current value because the old
  one was destroyed, not because supersession was modelled.

## Decisions taken

Two, both settled before design:

1. **Old values are kept and remain searchable.** Not audit-only.
2. **They are opt-in per query, off by default.** The hooks that inject
   memory into every prompt see current values only. A caller asks for
   history explicitly.

The second decision exists because of the risk the first creates: an
agent shown "cycles to work" and "takes the tram to work" in one context
has to work out which is true. Keeping history out of the default path
means no existing prompt changes shape.

## Schema

One migration, `0010_fact_history`. The index changes from *one row per
key* to *one **current** row per key*:

```sql
DROP INDEX facts_identity;
CREATE UNIQUE INDEX facts_identity
    ON facts (user_id, coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid), fact_scope, key)
    WHERE deleted_at IS NULL AND superseded_by IS NULL;
```

Superseded rows are exempt from the constraint, which is what lets
history accumulate. Existing rows all have `superseded_by IS NULL`, so
the new index covers exactly the same set on an existing install and the
migration is a rebuild, not a data change.

Down migration restores the original predicate. It will fail if history
already exists for a key, which is correct: the old shape cannot
represent two versions, and silently discarding them would be worse.

## Write path

`UpsertFact` stops being an upsert. In one transaction:

1. `SELECT … FOR UPDATE` the current row for
   `(scope, fact_scope, key)`.
2. No current row → insert.
3. Value identical (`jsonb` equality, key-order-insensitive) → update in
   place, as today. A repeated mention must not create a version.
4. Value differs → insert the new row, then
   `UPDATE facts SET superseded_by = <new id>, invalidated_at = now(),
   valid_to = now() WHERE id = <old id>`.

`FOR UPDATE` plus the partial unique index handles concurrent writers,
which matters now that extraction runs eight-wide by default in
benchmarks (`worker.extract_concurrency`). Two racing writers of the same
key serialise on the lock; if one still loses, the partial index rejects
the second current row rather than allowing two.

Two rules landed earlier survive unchanged and are load-bearing here:

- **Provenance follows the value** (`aabb82f`). The new row is authored
  by the source that supplied the new value. The superseded row keeps
  the source that authored *it*, which is now permanently correct rather
  than merely current.
- **A changed value clears its embedding** (`3666b2e`). The new row gets
  a NULL embedding and the backfill worker embeds it. The superseded row
  keeps its own embedding, which correctly describes its own value —
  this is what makes historical rows searchable at all.

## Read path

Thirteen queries across nine files read `facts`:

```
internal/decay/decay.go:95          internal/store/facts.go:124,150,188
internal/retrieval/graph.go:242     internal/store/browse.go:168
internal/retrieval/retrieval.go:422,443   internal/store/identity.go:25
internal/store/stats.go:193         internal/store/sources.go:134
internal/store/directory.go:50,56
```

Each gains `superseded_by IS NULL` alongside its existing
`deleted_at IS NULL`, **except** the two retrieval queries, which gain it
conditionally.

This is the safety property of the whole design: every reader behaves
exactly as it does today, and history is invisible until something asks
for it. A missed reader is a bug that shows up as stale values leaking
into a prompt, so the reader audit is the risk to manage, not the schema.

`retrieval.Query` gains `IncludeHistory bool`, plumbed through
`httpapi.HookEvent` as `include_history`, exactly mirroring the existing
`OnlyRaw`. Default false. `SessionStart` and `UserPromptSubmit` leave it
false; `anamnesia_search` and the CLI let the caller choose.

No new fields on `SearchHit`. `Fact` already carries `ValidTo`,
`InvalidatedAt` and `SupersededBy`, so a consumer can already tell a
historical value from a current one and date it.

`stats` reports current facts, so an install's fact count does not
silently inflate as history accrues.

## Testing

DB-backed, following `internal/store/facts_test.go` and
`internal/retrieval/vector_test.go`. Written first, each watched to fail.

Store:
- a changed value creates a second row and supersedes the first, with
  `superseded_by`, `valid_to` and `invalidated_at` all set
- an unchanged value updates in place and creates no version
- the superseded row keeps its own value, source and embedding
- the new row is authored by the new source and has a NULL embedding
- `GetFact`, `ListFacts` and browse return only current rows
- the partial index rejects a second current row for one key
- concurrent upserts of one key leave exactly one current row

Retrieval:
- superseded facts are absent by default
- `IncludeHistory` returns them, and the current value is still present
- a historical hit carries the dates a caller needs to label it

## Not in scope

**Pruning.** History grows without bound. The `forget` worker could TTL
superseded rows later; a policy guessed at now would be guessed wrong.
Revisit when real growth is observable.

**A dedicated history API.** Search with the flag covers the use case. An
`anamnesia facts history <key>` command is a small follow-up once we know
how it is actually used.

**Experiences.** They already have `SupersedeExperience`. This spec does
not change them.
