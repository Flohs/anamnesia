# Design: fill the graph, and let retrieval walk it

Written 2026-08-18 against `v0.1.0-rc7` (commit f9bb1be).

Depends on [the retrieval eval](./2026-08-18-retrieval-eval-design.md). The
whole justification for this work is better recall, and without the eval that
claim cannot be checked.

**Reordered 2026-08-18: this is now third.** [Segmented
ingest](./2026-08-18-segmented-ingest-design.md) comes first, because a graph
built over 21 rows connects almost nothing. A denser corpus makes this design
worth more, not less.

---

## The problem

Two of the five memory domains have never held a row. On a live install:

```
facts 11   experiences 25   entities 0   edges 0   working_memory 0
```

The graph is empty and cannot fill itself. The extractor's operation enum is
`ADD_FACT | UPDATE_FACT | DELETE_FACT | ADD_EXPERIENCE | NOOP` (plus
`ADD_COMMITMENT` when enabled) — `extract.go:656`. **No operation produces an
entity or an edge.** The only writers are the MCP tools `anamnesia_graph_entity`
and `anamnesia_graph_edge` (`mcp/server.go:823,851`), which fire only when
Claude deliberately decides to, and nothing prompts it to.

Meanwhile `internal/retrieval` never reads `entities` or `edges` at all. So
even a populated graph would change no result.

What exists and is unused: the tables, the HNSW index on `entities.embedding`,
four bitemporal timestamps per edge with their indexes, `UpsertEntity`,
`CreateEdge`, `InvalidateEdge`, `Neighbors`, and three MCP tools.

## The linkage problem

`edges` reference `entities(id)` at both ends and nothing else. **There is no
column anywhere joining a fact or an experience to an entity.** The graph is
an island: even fully populated, there is no path from "this experience ranked
highly" to "what else relates to it".

Both `facts` and `experiences` carry `source_id` (migration `0003`). That is
the bridge this design uses, and it needs one new table rather than changes to
existing ones:

```sql
-- 0009_entity_mentions.sql
CREATE TABLE entity_mentions (
    entity_id  UUID NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    source_id  UUID NOT NULL REFERENCES sources(id)  ON DELETE CASCADE,
    PRIMARY KEY (entity_id, source_id)
);
CREATE INDEX entity_mentions_source ON entity_mentions (source_id);
```

An entity is upserted once and mentioned by many sources, so a `source_id`
column on `entities` would be wrong; the join table is the honest shape. It
also gives the traversal its return path: entity → sources that mention it →
the facts and experiences extracted from those sources.

## Part 1 — extraction

A **second LLM pass**, behind a config flag, following the exact shape
`ExtractCommitments` already established.

```
source → gate → candidates ─┬─ pass 1  ADD_FACT / ADD_EXPERIENCE   (unchanged)
                            └─ pass 2  ADD_ENTITY / ADD_EDGE       (graph.extract)
```

A second call rather than four ops in one, because pass 1's prompt already
carries a default-to-NOOP prior, an 8-operation cap and list-decomposition
rules. Adding a second extraction task to it would have relationships compete
with facts for the same budget, and a malformed graph response would take the
facts down with it. Separate calls fail separately.

New settings, declared in `settings.go` like everything else:

- `graph.extract` (bool, default **false**) — run the second pass
- `graph.max_ops` (int, default 12) — cap per source

Default off. It doubles per-source model cost, and an install that never looks
at the graph should not pay for it.

### The operations

```json
{"op": "ADD_ENTITY", "kind": "service", "name": "stock-reconciliation",
 "props": {"role": "nightly job"}}
{"op": "ADD_EDGE", "from": "stock-reconciliation", "to": "rotterdam-warehouse",
 "kind": "reads_from", "trust": 0.8}
```

Edges name entities by **name, not id**. The model has never seen a uuid and
cannot invent one correctly; the executor resolves names against entities
upserted in the same pass and against existing entities in scope, and drops an
edge whose endpoints do not resolve. A dropped edge is recorded on the trace,
not silently discarded.

`ADD_ENTITY` upserts on `(scope, kind, name)` — the existing unique index
`entities_identity` and `UpsertEntity`'s conflict target already agree on
exactly that tuple. But they dedupe on the **literal** name, so normalisation
is the executor's job and must happen before the store call, or "the Rotterdam
warehouse", "Rotterdam warehouse" and "rotterdam warehouse" become three nodes
that the database is perfectly happy with. Entity resolution beyond
normalisation —
embedding-similarity merging — is explicitly **out of scope**; it is a
research problem and the eval will say whether it is needed.

### Superseding

The bitemporal machinery exists and is the point of the domain. When the pass
emits an edge whose `(from, to, kind)` already has a currently-valid row with a
different object — the `prefers(meat)` → `prefers(vegetarian)` case — the
executor calls `InvalidateEdge` on the old one and creates the new one, rather
than deleting anything. `valid_to` is set server-side with `now()`; the model
never supplies timestamps.

### Tracing

A `graph` step on the ingest trace, carrying the entities upserted, the edges
created, the edges superseded and the edges dropped for unresolvable
endpoints. Consistent with every other extraction stage, and the only way to
see why the graph looks the way it does.

## Part 2 — retrieval

Neighbour expansion as a **third ranked channel**, fused by the same RRF that
already merges vector and lexical:

```
vector  ─┐
lexical ─┼→ RRF k=60 → decay boost → rerank → top-K
graph   ─┘
```

The walk, given the fused top `GraphSeedN` hits (default 5):

1. Collect their `source_id`s.
2. `entity_mentions` → the entities those sources mention.
3. `Neighbors(entity, kinds=nil, direction="both", limit=GraphFanout)`,
   restricted to edges valid **now** (`valid_to IS NULL AND invalidated_at IS
   NULL`).
4. `entity_mentions` back → the sources mentioning those neighbour entities.
5. Facts and experiences from those sources, in edge-trust order, as the graph
   channel's ranking.

New `Query` fields, defaulted so existing callers are untouched:

- `GraphSeedN` (default 5, 0 disables)
- `GraphFanout` (default 10)
- `GraphK` (default 20) — candidates the channel contributes

One hop only. Two hops on a small graph reaches most of it and turns
expansion into noise; if the eval shows one hop helping, two becomes a
question worth asking with evidence.

The channel is skipped entirely when the graph is empty, which is both the
current state and the state of every install with `graph.extract` off. That
must cost nothing measurable: a single `COUNT` guarded by the existing scope,
or better, skip when step 2 returns no entities.

### Why this ordering matters

Expansion runs **after** fusion, not before. Seeds are the rows the existing
search already believes in, so the graph adds reachable-and-related rows rather
than replacing the ranking with a graph walk. If the graph is wrong, results
degrade gracefully instead of catastrophically.

## Testing

Store-level, DB-backed (skipped without `ANAMNESIA_TEST_DATABASE_URL`, as
`consolidate_trace_test.go` already does):

- upsert dedupes on normalised name; three spellings make one entity
- an edge whose endpoints do not resolve is dropped, and the trace says so
- superseding closes the old edge with `valid_to` and leaves it readable
- `Neighbors` excludes invalidated edges

Retrieval-level:

- with an empty graph, results are byte-identical to today's — this is the
  regression that matters most, since the channel ships enabled-by-default on
  the read side
- a seeded two-entity graph surfaces a row that neither vector nor lexical
  search returns, which is the entire claim of this design in one test

Extraction-level, no DB: the pass-2 prompt and schema are only sent when
`graph.extract` is set, mirroring `TestCommitmentPromptGating`.

Then the eval, before and after, quoted in the commit.

## Rollout

1. Migration `0009`, store methods for `entity_mentions`.
2. Extraction pass behind `graph.extract`, default off.
3. Retrieval channel, no-op while the graph is empty.
4. Eval run with `graph.extract` on against a corpus with multi-hop questions
   in it. If recall does not move, the honest outcome is to say so in the
   changelog and leave the flag off by default.

## Open questions for review

- Should `graph.extract` run on the liberal/benchmark prompt path too, or only
  the default one?
- Should entities be embedded on write (the column exists) even though this
  design never searches them directly? It costs an embed call per new entity
  and would enable entity-anchored search later.
