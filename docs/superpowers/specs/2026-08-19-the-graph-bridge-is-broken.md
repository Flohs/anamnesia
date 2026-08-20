# Finding: the graph retrieval channel can never return a result

Written 2026-08-19, from measuring the graph work on branch
`feat/graph-extraction`. **The branch is not merged.** This records what is
wrong, why it survived four reviews, and what fixing it takes.

---

## The defect

The graph retrieval channel is inert in production. Not "until the graph is
populated" — always, regardless of what the graph contains.

```
runGraph records mentions against src.ID              extract/graph.go:261
  where src is the "claude-session-graph" source,
  whose op enum is ADD_ENTITY | ADD_EDGE | NOOP       extract/graph.go:54
  -> a graph source never produces a fact or experience

graphExpand seeds from the fused hits' source_ids     retrieval/graph.go
  which are always fact/experience sources, i.e. the segment sources
  and EntitiesForSources matches source_id exactly    store/graph.go:341

=> entity_mentions holds only graph-source ids
=> hits carry only segment-source ids
=> the seed join can never match
```

Confirmed in Postgres, not only by reading: the two id sets are disjoint, and
a real 18-turn transcript that produced 4 entities and 2 edges surfaced no
graph hit for any query.

## Why it exists

The original design ran the graph pass **per source**, on the same source that
produced facts and experiences. Mentions and hits therefore shared source ids
by construction, and the bridge worked without anyone having to think about it.

On 2026-08-19 that was revised to run the pass **once per checkpoint**, for two
good reasons: segmented ingest had turned one long session into ~25 sources, so
a per-source graph pass meant ~25 extra model calls; and segments are cut at
topic boundaries, so a per-segment pass could only ever emit edges between
entities appearing in the same segment, which defeats the point of a graph.

The revision was right. What it missed is that routing the graph pass through
its own synthetic source severed the only thing connecting entities to memory
rows. The revision note reasoned carefully about cost and about edge quality,
and not at all about the bridge it was standing on.

## Why four reviews missed it

Every review was scoped to one task's diff, and this defect exists only in the
seam between two of them: Task 2 writes mentions, Task 4 reads them, and each
is internally consistent. Nothing in either diff is wrong.

The test that should have caught it — Test B, "the graph surfaces a row neither
search finds" — passes because its fixture records mentions **by hand** against
sources that also carry facts. That is a situation production cannot produce.
The test proves the walk works given a correct bridge; it cannot notice that
nothing builds one.

Worth stating plainly, because it generalises: a test whose fixture is
assembled by the test rather than by the system under test will not detect a
system that assembles it differently.

## What a fix takes

Not a one-liner. The hook already knows each segment's source id and throws it
away — `httpPost(ctx, hc, "/v1/ingest", payload, nil)` passes `nil` for the
response rather than decoding `IngestResponse.SourceID`.

1. `doCheckpoint` captures each segment's `source_id` as it posts.
2. It passes them on the graph source, e.g. `metadata.segment_source_ids`.
3. `runGraph` records a mention for every entity against **every** segment
   source of that checkpoint, not against its own synthetic source.

Then a hit on any segment of a checkpoint seeds the walk into the entities that
checkpoint mentioned, which is what the design always intended.

Two things to decide while doing it:

- **Should the graph source keep a mention of its own?** Probably yes, for
  provenance, but it must not be the only one.
- **Should mentions be per-segment-accurate rather than per-checkpoint?** An
  entity named only in segment 3 would be recorded against all N segments under
  the simple fix. More precise attribution needs the graph pass to know which
  segment each mention came from, which it currently cannot.

## What was measured, and stands regardless

- **Extraction works.** An 18-turn, 3-subject transcript produced 4 entities
  and 2 edges with sensible names and kinds, and correctly dropped 2 edges
  whose endpoints were named but never declared.
- **The empty-graph path is inert.** p50 856ms on an empty scope against
  1233ms on a populated one, but a zero-entity 40-source scope also ran
  1222ms — the difference is corpus size and rerank cost, not graph overhead.
- **`anamnesia eval` cannot measure the graph at all.** It posts
  `kind="chat-turn"` and never `claude-session-graph`, so the graph pass has no
  code path into it. Any before/after comparison through the eval is
  meaningless by construction — a fact the plan and its dispatch both got
  wrong.

## Unrelated finding, worth its own look

The eval's run-to-run variance has grown far beyond what was measured this
morning: recall@5 of 0.38 / 0.54 / 0.80 across identical corpora, against a
0.860–0.920 band from six runs earlier the same day, with extracted fact counts
varying 14/18/16/16. `regressionTolerance` is currently 0.05, set from a
standard deviation of 0.021 that no longer holds. Nothing on this branch
touches the eval path. See
[the eval follow-up](./2026-08-18-eval-drain-and-variance-followup.md).
