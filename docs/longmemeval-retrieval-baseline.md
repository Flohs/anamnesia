# LongMemEval retrieval baseline

Recorded 2026-08-21. Machine-readable copy:
[`superpowers/plans/lme-retrieval-baseline-2026-08-21.json`](./superpowers/plans/lme-retrieval-baseline-2026-08-21.json).

This is **not** a LongMemEval score. It measures retrieval alone: whether
the sessions holding a question's evidence come back in the top K. There
is no answerer and no judge, so it costs one `/v1/retrieve` call per
question and carries none of their variance. It cannot be compared to any
published LongMemEval number.

## The numbers

30 questions, 56 gold evidence sessions, all scored.

```
recall@1  0.334    recall@5  0.688    recall@10  0.871    recall@20  0.871    MRR  0.667
```

| evidence status | count | what it means |
|---|---|---|
| `retrieved` | 49 (87.5%) | the gold session's rows ranked |
| `stored_not_retrieved` | 3 (5.4%) | a miss with no capture verdict, or rows carry the answer |
| `answer_elsewhere` | 3 (5.4%) | captured, but attributed to another source |
| `answer_missing` | 1 (1.8%) | no row anywhere carries the answer |
| `not_stored` | 0 | |
| `not_ingested` | 0 | |

The recall figures moved slightly from the first cut (0.854 to 0.871)
because retrying the credit-failed sources added facts to fifteen of the
questions. That is corpus growth, not the classifier: capture analysis
cannot affect recall@k, which is computed purely from ranked source ids
against gold ids.

recall@20 by ability (2 to 8 questions each, indicative only):

```
knowledge-update           n=5   1.000
single-session-preference  n=2   1.000
multi-session              n=8   0.871
temporal-reasoning         n=8   0.833
single-session-user        n=4   0.750
single-session-assistant   n=3   0.667
```

Retrieval channels, over 600 hits:

```
vector      563  (93.8%)
lexical       0  ( 0.0%)
graph        64  (10.7%)   of which graph_only 37 (6.2%)
reranked    600  (100.0%)
```

## What it says

**Provenance and ranking are comparable, at 3 misses each.** An earlier
version of this file claimed provenance dominated 6:1. That was wrong,
and the story of how is worth keeping. The capture analysis called a miss
`answer_elsewhere` whenever *any* content word of the gold answer
appeared anywhere in the store, and gold answers are prose. An abstention
question matched on "any"/"did"/"not"; a derived answer ("14 days")
matched on "day"/"days"/"last"; a security answer matched
"one"/"two"/"time" while missing "biometric", "otp" and "authentication".
Five of six verdicts were manufactured by the measure. It now requires a
term that is rare in the question's own corpus, gives no capture verdict
for abstention questions or answers with no distinctive terms, and says
`stored_not_retrieved` (a miss, cause unknown) rather than inventing one.

Of the three surviving `answer_elsewhere` verdicts, two are solid
(`page`/`turners` both present; `brookside`/`condo`/`kitchen`/`highway`
present) and one is borderline (matches `factor`/`methods`/`passwords`
but still misses `biometric`/`otp`/`authentication`).

So the `ADD_FACT` collision path in `UpsertFact`, which moves `source_id`
via `COALESCE(EXCLUDED.source_id, facts.source_id)`, is still worth
finishing, but it is not the landslide the first cut suggested. The no-op
`UPDATE_FACT` path was fixed on 2026-08-21 and moved the corpus-wide
misattribution rate from 35.8% to 31.3%.

**Ranking is not obviously the problem either.** Three
`stored_not_retrieved` in 56, and two of those three are questions where
capture analysis abstained rather than confirmed the rows were present.

**`recall@10` equals `recall@20` exactly.** Nothing arrives between ranks
11 and 20. With `recall@1` at 0.346, evidence is either found early or
not at all. The hand-built corpus behind `anamnesia eval` shows the same
signature one cutoff lower (`recall@5 == recall@10`), so this is a
property of the pipeline rather than of one corpus.

**The graph contributes.** 37 of its 64 hits were reached by no other
channel, across 19 of 30 questions. Note this requires `graph.extract` on
**and** the harness posting `claude-session-graph` sources; without the
second, the graph pass never runs and the channel reports a structural 0.

**The lexical channel is inert.** Zero hits in 600, across four runs and
every question type. The channel works on literal terms, but extraction
rewrites prose into keys like `user.recent_audition.play` and JSON
values, so `plainto_tsquery` has nothing to match a natural-language
question against. RRF is effectively vector-plus-graph on extracted
memory.

## Reproducing and comparing

The corpus is defined by [`../scripts/longmemeval/subset-30.txt`](../scripts/longmemeval/subset-30.txt):
30 questions from `longmemeval_s_cleaned`, stratified proportionally by
`question_type`, ordered by `sha256(question_id)` so the selection is
reproducible and independent of file order.

```bash
python scripts/longmemeval/harness.py \
  --dataset ./data/longmemeval_s_cleaned.json \
  --mode retrieval --retrieve-k 20 \
  --out ./out/lme-retrieval.jsonl
```

**A retrieval-ranking change does not need a re-ingest.** Re-score the
stored corpus with `--skip-ingest`: no model calls, so the comparison is
deterministic and paired against this baseline. That is the only kind of
comparison here that is free of noise.

**An extraction change does need a re-ingest**, and then the comparison is
*not* paired: extraction is nondeterministic, and two runs of one corpus
differ. Do not read a small delta as a result. Prefer the corpus-wide
misattribution measure or another aggregate over hundreds of rows to a
recall delta over 56 evidence sessions.

## Provenance of this run

Recorded with `worker.extract_concurrency=8`, `worker.embed_backfill=2s`,
`worker.extract_every=1s`, `graph.extract=true`, `retrieve_k=20`, against
`openai/gpt-4o-mini` and `text-embedding-3-small` (1536) via OpenRouter,
with `cohere/rerank-v3.5`. Schema v9.

Final corpus: 2,872 sources (2,764 done, 100 skipped, 8 failed), 6,342
facts, 1,185 experiences, 5,808 entities, 2,812 edges.

Two things happened during the run that matter for interpreting it:

- The OpenRouter account ran out of credits partway through, failing 669
  sources across seven questions. They were recovered by resetting those
  rows to `pending` rather than re-POSTing, which preserved their
  `external_ref` and the graph sources' `segment_source_ids`. All seven
  came back with 169-268 facts each.
- Three questions have integer gold answers and crashed the scorer
  (`answer_terms` assumed a string). Fixed, and those three were
  re-scored with `--skip-ingest` against the same corpus.

The 0.3% final extraction failure rate (8 of 2,872) is much lower than
the ~14% malformed-JSON rate observed early in the session, so the
extractor's reliability with `gpt-4o-mini` is not stable across runs and
is worth watching.
