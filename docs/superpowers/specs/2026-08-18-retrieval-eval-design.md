# Design: a retrieval eval that makes changes decidable

Written 2026-08-18 against `v0.1.0-rc7` (commit f9bb1be).

**Build this first.** The other two designs — [graph
extraction](./2026-08-18-graph-extraction-design.md) and [working-memory
folding](./2026-08-18-working-memory-fold-design.md) — both claim to improve
retrieval, and neither claim can currently be checked.

---

## The problem

There is no way to tell whether a retrieval change helped.

`scripts/longmemeval/harness.py` exists and is real, but it is end-to-end: it
ingests a haystack, retrieves, asks a model to answer from the hits, and asks a
judge to score the answer. That measures the whole pipeline through two LLM
calls per question. It cannot say whether *retrieval* got better, it takes
hours, and its numbers move for reasons that have nothing to do with ranking.

The consequences are already visible. The extraction prompt was changed in rc5
to ask for experience titles, partly on the argument that `experiences.title`
feeds the lexical tsvector and an untitled row is a weaker search target. That
argument was never tested. We verified a title appeared; we did not verify
anything was easier to find. Every retrieval decision in the tree — RRF over
score normalisation, `RRFConst = 60`, `VectorK`/`LexicalK` of 40, the
decay-multiply in `fuse`, reranking the top 4×K — rests on the same kind of
reasoning.

## What this is not

Not a benchmark, not a leaderboard number, and not a replacement for
LongMemEval. It is a regression harness for one question: **did this change
make the right rows rank higher?**

## Design

### The corpus

Two files under `testdata/eval/`, both JSONL, both committed and reviewable in
a diff.

`corpus.jsonl` — sources to ingest, roughly 60, each with a stable id:

```json
{"id": "src-014", "kind": "chat-turn", "occurred_at": "2026-03-02T09:14:00Z",
 "content": "The Rotterdam warehouse writes timestamps in local time while every other site writes UTC …"}
```

`queries.jsonl` — labelled queries, roughly 40:

```json
{"id": "q-007", "text": "why were the nightly stock counts off by a day",
 "relevant": ["src-014", "src-021"], "note": "the timezone cause, not the fix"}
```

Relevance is at **source** granularity, not row granularity. A source produces
an unpredictable number of facts and experiences, and pinning labels to
extracted row ids would make the gold set a hostage of the extraction prompt —
every prompt change would invalidate it. A hit counts as relevant when its
`source_id` is in the query's `relevant` set.

The corpus must contain **near misses on purpose**: sources that share
vocabulary with a query but do not answer it. A corpus where every relevant
source is also the only one mentioning the query's nouns measures nothing —
lexical search alone would score perfectly.

### The metrics

Per query, from one `/v1/retrieve` call:

- `recall@1`, `recall@5`, `recall@10` — fraction of the relevant set retrieved
- `MRR` — reciprocal rank of the first relevant hit
- latency `p50`, `p95`

Reported per query and aggregated. Also reported: how many queries returned
**nothing** relevant at all, which is the number that actually matters for an
agent — a query that finds nothing is a session where memory silently did not
work.

### The command

`anamnesia eval`, a new subcommand in `cmd/anamnesia`, not a separate binary.
The site claims a `cmd/anamnesia-eval`; a second binary would be a second
thing to build, install and keep in sync, against a project whose stated shape
is one binary.

```
anamnesia eval                       run against the configured stack
anamnesia eval --json                machine-readable, for diffing runs
anamnesia eval --baseline out.json   compare against a previous run and
                                     exit non-zero if recall@5 regressed
anamnesia eval --keep                leave the ingested corpus in place
```

It refuses to run against the default scope. The corpus is ingested under a
dedicated user (`eval`) and project (`eval-corpus`), and `--keep` aside, it
deletes them afterwards. Running an eval must not be able to pollute the
memory of the person running it.

### How a run works

1. Resolve the eval scope, deleting any leftovers from a previous run.
2. Ingest every corpus source through the normal `/v1/ingest` path.
3. Wait for `extract_pending` and `embed_pending` to reach zero, polling
   `/v1/queue/pending`. Fail with a clear message on timeout rather than
   scoring a half-warm index.
4. For each query, call `/v1/retrieve` with `only_raw=true` and `k=10`, timing
   it.
5. Score, aggregate, print.
6. Delete the eval scope unless `--keep`.

Step 3 is the step that makes the numbers mean anything, and it is exactly
what the `queues` event added in rc7 already reports.

### What it costs to run

Real extraction and real embeddings, so a real model key. Roughly 60
extractions plus 60 embeds plus 40 query embeds per run. This is why it is
**not wired into CI**: CI has no key, no database and no business spending
money per push. It is a command a person runs before and after a retrieval
change, and quotes in the commit message.

## Testing

The harness needs tests of its own, because a scorer that is wrong is worse
than no scorer:

- `recall@k` and `MRR` against hand-computed fixtures, including the
  degenerate cases: no relevant hits, all relevant hits, relevant hit at rank
  1, duplicate `source_id`s across hits.
- The corpus and query files parse, every `relevant` id exists in the corpus,
  and no query has an empty relevant set. This runs in CI — it needs no
  database, and a mislabelled gold set is a silent, permanent error.

## What this unlocks

Both other designs become decidable. "Graph expansion improved recall@5 from
0.62 to 0.71 on the fixture corpus" is a sentence worth putting in a commit
message. "It should help" is not.

## Open question for review

Whether the corpus should be written by hand or distilled from real sessions.
Hand-written is reviewable and shareable; distilled is more representative and
cannot be published. This design assumes hand-written.
