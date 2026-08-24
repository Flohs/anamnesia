# Extraction

What turns a checkpoint into memory. This is the part that decides what you
keep, so it is the part most worth understanding.

The governing principle: **the default answer is `NOOP`.** Most of what you
say to an assistant is noise, and a memory system that keeps it all is a
transcript archive with extra steps.

## The pipeline

A checkpoint lands as a `sources` row. Every `worker.extract_every` (15s) a
background worker drains pending sources through five steps.

```
source row
   │
   ├─ 0. too short?  ────────────────────────► skipped
   │
   ├─ 1. surprise gate ──── already covered ─► skipped
   │
   ├─ 2. candidate fetch (hybrid search, K=5)
   │
   ├─ 3. one model call: content + candidates → operations
   │
   └─ 4. execute: ADD_FACT / UPDATE_FACT / DELETE_FACT
                  ADD_EXPERIENCE / NOOP
```

Sources shorter than 16 characters are dropped before anything else runs.

## Step 1: the surprise gate

The gate asks one question: does an existing memory already cover this? It is
deliberately cheap, one search at K=1, because it runs on everything and its
job is to avoid the expensive step that follows.

It **skips** extraction when the nearest existing memory scores at or above
`SurpriseThreshold` (0.93). High enough that a paraphrase still gets through.

Three things bypass the gate entirely:

- **A temporal marker in the content.** If you are telling the agent that
  something just changed, similarity to what it used to be is not a reason to
  ignore you.
- **An evaluation source kind**, so benchmark streams retain every passing
  mention.
- **A gate failure.** If the gate cannot run, extraction continues anyway.
  Failing open is right here: the cost of a redundant extraction is one model
  call, and the cost of failing closed is silently losing memory.

**The gate needs a reranker to skip anything.** RRF scores are relative, not
absolute, so without a reranker the gate has no absolute score to judge with
and always extracts. With `rerank.provider` unset you are paying for
extraction on content you already have. That is the conservative direction,
and it is why turning on a reranker reduces model spend rather than adding to
it.

## Step 2: candidates

A hybrid search fetches the 5 most similar existing facts and experiences and
sends them along with the content. This is what makes `UPDATE_FACT` possible:
the model cannot supersede a value it has not been shown.

Candidate fetch failing is soft. Extraction proceeds without candidates rather
than blocking ingest, which trades a possible duplicate for a guaranteed loss.

This is also why `worker.extract_concurrency` defaults to 1. Sources handled
at the same time stop seeing each other's new facts as merge candidates, so a
bulk backfill of related sessions can produce duplicates a serial drain would
have merged. Raise it for benchmarks and backfills; leave it at 1 for a live
install where dedup matters more than throughput.

## Step 3: the model call

One call, with the content and the candidates, returning a list of operations.
Capped at 8 operations per source, which protects against runaway output.

| Operation | Effect |
|---|---|
| `ADD_FACT` | New keyed claim |
| `UPDATE_FACT` | Supersede an existing fact, keeping the old row |
| `DELETE_FACT` | Soft-delete |
| `ADD_EXPERIENCE` | New time-stamped narrative |
| `NOOP` | Nothing here is worth keeping. The default |

With `worker.extract_commitments` on, `ADD_COMMITMENT` joins the list. It is
off by default so the prompt and schema stay smaller for installs that do not
want a commitments ledger.

`UPDATE_FACT` does not overwrite. The old row keeps its text, provenance and
embedding, and gains `valid_to` and `superseded_by`. See
[Memory model](memory-model.md#facts).

## Graph extraction

A separate job on the same queue, off by default (`graph.extract`). It carries
the whole checkpoint's text, posted once per checkpoint rather than once per
segment, and takes none of the path above: no surprise gate, no candidate
fetch, no fact pass.

It produces entities and edges, capped by `graph.max_ops` (12).

Deciding whether a newly extracted entity is one you already have is a
model's judgement, not a threshold. `graph.candidate_distance` (0.45) only
controls which existing entities are offered as possible matches, triggering
one extra model call to ask whether two names mean the same thing. It is
deliberately loose, because it does not merge anything by itself.

## Segmentation, and why it changes what you get

Checkpoints are cut into segments before they are sent, so the gate judges one
subject at a time. The size cap is measured:

On three real 21KB sessions, the same content yielded **14 unique facts at a
32768-byte cap and 74 at 4000**. The facts only the smaller cap found were
standing preferences like "branch instead of committing directly to main",
which is exactly what memory is for.

Attention degrades over a long input, so a bigger segment does not extract
more, it extracts less. The cost is one model call per segment: 3 calls became
18. See [Hooks](hooks.md#segmentation).

## What extraction is bad at

This is measured, and it is the honest weak point.

Over a 30-question LongMemEval corpus, retrieval scored `recall@5` **0.956**
and end-to-end answers scored **46.7%**. The gap is not a rounding error, and
it is extraction's.

A real failure, in full:

> **Question** Where do I currently keep my old sneakers?
> **Gold** in a shoe rack in my closet
> **Anamnesia** in a shoe rack
> **Judge** No.

Retrieval was perfect: the gold session came back ranked third, as
`user.organizing.shoe_rack → "store old sneakers in a shoe rack"`. Extraction
had dropped *"in my closet"*. The word appears in 14 raw sources and survives
in exactly one unrelated fact.

**Facts get captured but lose their qualifiers.** A memory that says "in a
shoe rack" when you asked *where* something is has lost the useful half. That
is a different problem from facts being dropped entirely, which segmentation
fixed, and it is open.

Full record: [the LongMemEval baseline](../longmemeval-retrieval-baseline.md).

## Watching it

```bash
anamnesia logs -f                     # the extractor, live
curl localhost:8181/v1/queue/pending  # how much is waiting
anamnesia doctor                      # queue depth as a check
```

Every extraction records a trace, viewable on `/v1/activity`, with a step per
stage and the gate's verdict and reason in plain words:

```
gate   Kept: the nearest memory scores 0.71, below the 0.93 threshold
gate   Skipped: an existing memory already covers this
gate   Kept: the content says something just changed
```

`activity.enabled` turns recording off; `activity.traces` (200) is how many
are kept. They live in memory only, so a restart clears them.

## When nothing is being extracted

In order of likelihood:

1. `llm.provider` is `stub`. There is no model. Check `anamnesia config`.
2. No embedder, so no candidates and no gate. Check `embed.provider`.
3. The queue is backed up. Check `/v1/queue/pending`.
4. It is working and the answer is genuinely `NOOP`. Read the traces on
   `/v1/activity` and see what the gate said.

See [Troubleshooting](troubleshooting.md#nothing-is-being-remembered).
