# LongMemEval retrieval baseline

Recorded 2026-08-21, on a corpus ingested with **session segmentation**
(`--segment-bytes 4000`), the way `cmd/anamnesia/hook.go` cuts a
checkpoint. Earlier entries posted whole sessions, which is a path
production never takes. Machine-readable copy:
[`superpowers/plans/lme-retrieval-baseline-2026-08-21.json`](./superpowers/plans/lme-retrieval-baseline-2026-08-21.json).

This is **not** a LongMemEval score. It measures retrieval alone: whether
the sessions holding a question's evidence come back in the top K. There
is no answerer and no judge, so it costs one `/v1/retrieve` call per
question and carries none of their variance. It cannot be compared to any
published LongMemEval number.

## The numbers

30 questions, 56 gold evidence sessions, all scored.

```
recall@1  0.540    recall@5  0.956    recall@10  0.956    recall@20  0.956    MRR  0.928
```

| evidence status | count | what it means |
|---|---|---|
| `retrieved` | 54 (96.4%) | the gold session's rows ranked |
| `stored_not_retrieved` | 1 (1.8%) | a miss with no capture verdict, or rows carry the answer |
| `answer_elsewhere` | 1 (1.8%) | captured, but attributed to another source |
| `answer_missing` | 0 | no row anywhere carries the answer |
| `not_stored` | 0 | the session produced no rows at all |
| `not_ingested` | 0 | |

**End-to-end accuracy on the same corpus: 14/30 (46.7%)**, answerer and
judge both `gpt-4o`, using LongMemEval's own judge prompts. The gap
against 0.956 retrieval is the most useful number in this document:
retrieval hands over the right memory almost every time and the answer is
still wrong more often than not.

A worked example. "Where do I currently keep my old sneakers?" Gold: *in
a shoe rack in my closet*. Anamnesia answered *in a shoe rack*, judged
wrong. Retrieval was perfect — the gold session ranked third, as
`user.organizing.shoe_rack → "store old sneakers in a shoe rack"`.
Extraction had dropped "in my closet", a word that appears in 14 raw
sources and survives in one unrelated fact.

So retrieval is close to solved here and **extraction fidelity is not**:
facts are captured but lose their qualifiers. That is a different failure
from facts being dropped entirely, which segmentation fixed, and neither
the retrieval metric nor the capture analysis can see it — both check
whether the right *session* or *some* distinctive term came back, never
whether the whole answer survived.

By ability: single-session-preference 2/2, multi-session 5/8,
single-session-user 2/4, temporal-reasoning 3/8,
single-session-assistant 1/3, knowledge-update 1/5.

**Both write-path failure categories are zero.** Every one of the 56 gold
evidence sessions was extracted and stored; the two remaining misses are
ranking, not loss. That is the headline: on this corpus, extraction no
longer drops content.

| | whole sessions | segmented |
|---|---|---|
| recall@5 | 0.880 | **0.956** |
| recall@10 / @20 | 0.943 | **0.956** |
| MRR | 0.915 | **0.928** |
| `answer_missing` | 1 | **0** |
| `not_stored` | 1 | **0** |
| extraction failures | 32 | **0** |
| wall time | ~1h | ~3h45m |

**Do not attribute that delta to segmentation alone.** This is a rebuild,
so it is not paired: it carries segmentation *and* the extraction retry
and budget-escalation fixes, and extraction is nondeterministic. What
*is* directly attributable is the mechanism, verified in the database:
the gold fact for `58bf7951`, which whole-session extraction lost in 9
runs out of 10, is now extracted as `user.attended_play.glass_menagerie`
from segment `answer_355c48bb#0`.

**Earlier entries, same 30 questions.** Before the write-path fixes:
recall@1 0.334, recall@5 0.688, recall@10/@20 0.871, MRR 0.667,
`answer_elsewhere` 3. After them, still on whole sessions: recall@1
0.540, recall@5 0.880, recall@10/@20 0.943, MRR 0.915.

**Do not read that delta as the fixes' doing.** This corpus was rebuilt,
so the comparison is not paired: extraction is nondeterministic, and this
run had a 1.2% malformed-JSON rate against roughly 14% in the earliest
runs, so the corpus is simply better independent of any code change. A
0.25 jump in MRR is not attributable to two SQL predicates. Only
`--skip-ingest` re-scoring against a fixed corpus is a paired
comparison.

What *is* attributable is the write-path measure below, taken over 4,379
facts rather than 56 evidence sessions.

recall@20 by ability (2 to 8 questions each, indicative only):

```
knowledge-update           n=5   1.000
single-session-preference  n=2   1.000
single-session-user        n=4   1.000
temporal-reasoning         n=8   1.000
multi-session              n=8   0.958
single-session-assistant   n=3   0.667
```

Retrieval channels, over 600 hits:

```
vector      587  (97.8%)
lexical       2  ( 0.3%)
graph        74  (12.3%)   of which graph_only 13 (2.2%)
reranked    600  (100.0%)
```

## What it says

**Misattributed provenance: 35.8% → 31.3% → 20.5%.** The share of facts
attributed to a source containing no word of them, measured over 4,379
facts. The `UPDATE_FACT` fix (no-op updates taking authorship) took 4.5
points; the `ADD_FACT` fix (a repeated value taking authorship, `aabb82f`)
took a further 10.8. `answer_elsewhere` went 6 → 3 → **0**: no gold
evidence in this corpus is now lost to misattribution. Facts missing an
embedding: 0 of 6,293, so the re-embed-on-change fix (`3666b2e`) is not
stranding rows mid-backfill.

**Historical note on the previous entry.** An earlier
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

**The graph contributes.** 13 of its 74 hits were reached by no other
channel, across 21 of 30 questions. The `graph_only` share fell from 27
of 53: with finer segments the vector channel finds more of what the
graph used to reach alone, so the graph is confirming more and
contributing less. Note this requires `graph.extract` on
**and** the harness posting `claude-session-graph` sources; without the
second, the graph pass never runs and the channel reports a structural 0.

**The lexical channel is all but inert.** It managed 2 hits of 600 on the
segmented corpus, its first non-zero result here and still negligible;
every earlier run scored zero across every question type. Shorter
segments make keys and values that occasionally match a question's
literal wording, which is why it is no longer exactly zero.

Two independent defects caused it. `plainto_tsquery` ANDs every term, so
"What play did I attend at the local community theater?" becomes
`play & attend & local & community & theater` and asks one short
extracted fact to contain all of it: it returned *zero rows*, not zero
relevant rows, for all 30 questions. (`websearch_to_tsquery` is not a
fix; it ANDs identically.) And 99.6% of fact keys are dotted, which the
text-search parser reads as a single `host` token, so every term living
only in the key was unreachable.

Both were fixed on a branch and measured against this same stored corpus,
deterministically (two runs, byte-identical):

| | lexical dead | lexical fixed |
|---|---|---|
| lexical hits | 0 (0.0%) | 236 (39.3%) |
| recall@10 / @20 | 0.871 | 0.871 |
| recall@5 | 0.688 | 0.671 |
| MRR | 0.667 | 0.662 |
| graph hits / `graph_only` | 64 / 37 | 42 / 16 |
| evidence verdicts | 49/3/3/1 | 49/3/3/1 |

The channel went from dead to firing on 30/30 questions and found **no
gold evidence the vector channel was not already finding** — not one
evidence verdict changed. The whole `recall@5` loss is a single question
where a gold row slipped from the top 5 to the top 20.

Sweeping `LexicalK` did not rescue it, and falsified the obvious
hypothesis that throttling it would reduce displacement:

```
variant                   r@1    r@5   r@10   r@20    MRR   lex   vec   gph  gonly
lexical OFF (pre-fix)   0.334  0.688  0.871  0.871  0.667     0   563    64     37
LexicalK=5              0.334  0.671  0.854  0.854  0.660   121   575    50     22
LexicalK=10             0.334  0.671  0.854  0.854  0.659   180   576    40     16
LexicalK=20             0.334  0.671  0.854  0.854  0.658   219   572    42     16
LexicalK=40             0.334  0.671  0.871  0.871  0.662   236   571    42     16
```

Lower K is *worse*, not better: only the full K=40 recovers recall@10/@20.

A separate probe tested the one thing this benchmark cannot see, exact
rare-token lookup, on a fixed set of 75 single-token queries each naming
a term unique to one stored fact:

```
                     exact-term recall@20
lexical in fusion            0.947 (71/75)
lexical removed entirely     0.947 (71/75)
```

Identical. The vector channel finds all of them alone, so the "lexical is
for exact identifiers" argument does not survive measurement either. Note
that a *single-token* query produces no conjunction, so the AND defect
never affected this case: exact lookup was working the whole time.

The fix was therefore **not merged**. The channel is left dead, and
`CLAUDE.md` records why, along with the two reasons it cannot simply be
deleted: `DomainSkill` has no vector channel and is served only by
`lexicalSkills`, and the entire `internal/retrieval` test suite runs
without an embedder, so lexical is the only channel those tests can
produce hits with.

## Reproducing and comparing

The corpus is defined by [`../scripts/longmemeval/subset-30.txt`](../scripts/longmemeval/subset-30.txt):
30 questions from `longmemeval_s_cleaned`, stratified proportionally by
`question_type`, ordered by `sha256(question_id)` so the selection is
reproducible and independent of file order.

```bash
python scripts/longmemeval/harness.py \
  --dataset ./data/longmemeval_s_cleaned.json \
  --mode retrieval --retrieve-k 20 \
  --segment-bytes 4000 \
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

Final corpus: 8,560 sources (8,025 done, 535 skipped, **0 failed**),
13,633 current facts plus 465 superseded ones, 2,776 experiences, 13,666
entities, 5,964 edges. Built at commit `acfcd74` with
`--segment-bytes 4000`.

Segmentation roughly triples the source count (8,560 against 2,872 for
the same 30 questions) and the wall time with it, ~3h45m against ~1h.
That is the cost side of the trade.

**Zero extraction failures**, against 32 in the previous corpus and
roughly 14% earlier in the same day. Those were two faults wearing one
error: transient garbage, which a retry fixes, and truncation, which
needs a bigger budget. Both are handled in `internal/llm`, and the
instability that used to be the biggest confounder in this document is
no longer visible at this corpus size. A rebuild still is not a paired
comparison — extraction remains nondeterministic in *what* it selects,
just no longer in whether it succeeds — which is why `--skip-ingest`
re-scoring exists.

The 465 superseded facts are fact history (migration 0010) working on a
real corpus for the first time: values that changed during ingest and
were versioned rather than overwritten.

Earlier entries were built through an OpenRouter credit outage that
failed 669 sources across seven questions; those were recovered by
resetting the rows to `pending` rather than re-POSTing, preserving their
`external_ref` and the graph sources' `segment_source_ids`.
