# Retrieval

How memory comes back. Three channels, fused by reciprocal-rank fusion,
optionally reranked, with time-decayed scoring on experiences.

## The channels

| Channel | Backed by | Domains it serves |
|---|---|---|
| Vector | pgvector ANN over embeddings | facts, experiences, entities, artifacts |
| Lexical | Postgres `tsvector` full-text | facts, experiences, **skills** |
| Graph | a walk over `entities` / `edges` | whatever the walk reaches |

Vector and lexical run per domain and produce up to 40 candidates each. Their
ranks are fused with RRF at a constant of 60.

The graph channel runs after that fusion: the top 5 fused hits seed a walk
through `Store.Neighbors`, capped at a fanout per seed entity, and the sources
it reaches contribute up to 20 extra candidates folded back into the RRF
score.

All three are tunable per query (`VectorK`, `LexicalK`, `GraphSeedN`,
`GraphFanout`, `GraphK`, `RRFConst`). Set `GraphSeedN` to -1 to disable the
graph channel; zero means "unset" and gets the default.

## Reranking

`rerank.provider` adds a second pass over the fused candidates. It costs
latency and buys precision.

It also does something less obvious: **it is what makes the surprise gate
work.** RRF scores are rank-based, so without a reranker there is no absolute
number to compare against a threshold, and the gate always extracts. Turning
on a reranker therefore reduces model spend rather than adding to it. See
[Extraction](extraction.md#step-1-the-surprise-gate).

`SkipRerank` on a query returns the fused order as-is, and is set where
nothing reads the ordering, such as the extractor's candidate fetch.

## Decay

Experiences carry a `relevance` that a worker recomputes every
`worker.decay_every` (1h). It halves on a schedule set by the experience's
kind:

| Kind | Half-life | Why |
|---|---|---|
| `case` | 336h (2 weeks) | An episode. What you did last fortnight matters; last spring usually does not |
| `hybrid` | 1440h (2 months) | Part episode, part approach |
| `strategy` | 8760h (1 year) | A learned approach outlives the day you worked it out |

Facts do not decay. A preference does not become less true because time
passed; it becomes false when you supersede it, which is a different mechanism.

## Project scoping

Retrieval prefers the current project but also surfaces a small number of
relevant hits from your other projects, so a decision made elsewhere still
finds you. `ProjectIn` on a query controls this: empty with a project set
restricts to that project.

## History

Superseded facts are excluded by default. Pass `include_history` to get them.

This default matters more than it looks. A superseded value competing with the
current one in retrieval is worse than not storing history at all, because the
answer becomes non-deterministic in a way nobody can see.

## Retrieval must be able to fail

A configured embedder that errors returns an **error** from `Search`, not an
empty result set.

This is an invariant, and it comes from a real incident: a credit outage once
had `/v1/retrieve` answer `200` with no hits for a user holding hundreds of
fully-embedded facts. That is indistinguishable from "you have no such
memory", which is the worst possible failure mode for a memory system, because
it looks like an answer.

Having **no** embedder configured stays legitimate. That is the lexical-only
local setup, and it returns what lexical can find.

## The lexical channel

The full-text channel earns almost nothing on extracted memory. That is
measured, not assumed, and it is worth reading before touching it.

Measured 2026-08-21 over a 30-question LongMemEval corpus
([full record](../longmemeval-retrieval-baseline.md)):

- `plainto_tsquery` ANDs every term, so a natural-language question matched
  **0 of 600** hits.
- Making it OR its terms and indexing the words inside dotted keys lifted it
  to 236 of 600 hits and **changed recall by nothing at all**, while costing
  `recall@5` (0.688 to 0.671) and crowding the graph channel out of the fused
  top-20 (`graph_only` 37 to 16).
- No `LexicalK` from 5 to 40 made it a net win.
- Exact rare-token recall was **0.947 with it and 0.947 without**, so it does
  not earn its keep on identifier lookup either.

**It stays anyway, and deleting it is not free.** `DomainSkill` has no vector
channel at all and is served only by `lexicalSkills`. Every test in
`internal/retrieval` runs without an embedder, so lexical is the only channel
those tests can produce hits with. Removing it would take skills retrieval
with it and require rewriting the graph tests.

Do not "fix" or expand it without new evidence, and do not delete it either.

## Artifacts do not go through Search

Artifact matching deliberately bypasses fusion and uses raw cosine distance.

The reason is that an artifact is a link put in front of someone who did not
ask for one, so it has to clear an **absolute** bar rather than merely rank
first. A fused RRF score cannot express that: it gives the best of an
irrelevant pool exactly what it gives the best of a relevant one, and nothing
downstream can tell "this matches" from "this was the least bad".

`retrieval.artifact_max_distance` (0.60) is that bar. It is measured over 33
real artifacts with `openai/text-embedding-3-small`: matches a person would
call relevant scored 0.32 to 0.63, and the closest match for a prompt about
something else scored 0.759. The band between them is wide and 0.60 sits
inside it with no false positives.

Raise it toward 0.65 to catch the tail of the relevant range at the cost of
the occasional off-topic link. Set it to 0 to stop surfacing artifacts on
prompts entirely, leaving `anamnesia artifacts` and the session-start list.

**Recalibrate if you change embedding models.** The bands are a property of
the model, not of Anamnesia.

With no embedder there is nothing absolute to report, and the honest answer is
no artifacts rather than three arbitrary ones.

## Measured performance

30 questions, 56 gold evidence sessions, scored mechanically against gold ids
with no model in the loop:

```
recall@5  0.956    recall@20  0.956    MRR  0.928

retrieved             54  (96.4%)
stored_not_retrieved   1        ← ranking missed it
answer_elsewhere       1        ← stored under the wrong source
answer_missing         0        ← extraction dropped it
not_stored             0        ← the session produced nothing
```

End-to-end, with an answerer and LongMemEval's own judge: **46.7%**.

The gap is extraction's, not retrieval's. See
[Extraction](extraction.md#what-extraction-is-bad-at).

The evidence breakdown is the useful part. A score alone says a question
failed; these categories say which subsystem failed, and they have repeatedly
pointed somewhere other than the guess would have:

- a benchmark that reported "ranking missed it" when extraction had never
  stored the answer at all
- provenance quietly reassigned, so a session about a play owned a fact about
  a bike
- an embedder outage that looked exactly like an empty memory
- a full-text channel returning zero rows for every query, for months

## Running it yourself

```bash
anamnesia eval        # against the built-in fixture corpus
```

For LongMemEval, `scripts/longmemeval/` runs against a live stack.
`--mode retrieval` scores retrieval directly against gold evidence sessions,
with no answerer and no judge, so a run costs one search per question.

```bash
python scripts/longmemeval/harness.py --dataset ./data/longmemeval_s_cleaned.json \
  --mode retrieval --skip-ingest --retrieve-k 20 --out ./out/rescore.jsonl
```

Re-scoring a stored corpus takes about 25 seconds and no model calls.
