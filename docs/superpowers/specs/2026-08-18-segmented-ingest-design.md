# Design: gate ideas, not sessions

Written 2026-08-18 against `v0.1.0-rc7` (commit f9bb1be).

**Build this before the graph work.** Retrieval quality is downstream of what
was extracted, and today the extraction unit is wrong.

---

## The evidence

Six sources on a live install, all from one day:

| transcript | state | operations |
|-----------:|-------|-----------:|
|  81 KB | done | 3 |
| **329 KB** | **skipped** | **0** |
|  44 KB | done | 8 |
|  55 KB | done | 2 |
|  34 KB | done | 4 |
| 126 KB | done | 4 |

Roughly 670 KB of engineering work became 21 operations, and the largest
session produced nothing at all.

Two mechanisms cause this, and both are correct in intent and wrong in scale.

**The surprise gate decides per source.** `Extractor.Run` calls
`surpriseGate(ctx, src.Scope, content, threshold)` once, over the whole body,
and a skip verdict ends the run: `tr.End("skipped", …); return 0, nil`. A
329 KB session is not one claim. It is a day containing dozens, and one cosine
comparison against one top-1 neighbour decided the fate of all of them. The
gate was designed for a chat turn — "is this message a duplicate?" — and is
being asked "is this *day* a duplicate?", a question it cannot answer.

**`MaxOps` is 8.** The 44 KB source returned exactly 8, which is a ceiling
being hit rather than a need being met. A day of work can contribute at most
eight memories no matter what happened in it.

The write path is also rare: 112 `retrieve` hook runs against 6 `session-end`,
and `pre-compact` has never fired. Memory is read constantly and written a
handful of times a day. That makes each write disproportionately valuable, and
each one thrown away disproportionately expensive.

## The change

Cut a checkpoint into segments in the hook, and post each as its own source.

```
                          today                    after
SessionEnd  →  1 × POST /v1/ingest    →  N × POST /v1/ingest
                 1 gate verdict            N independent gate verdicts
                 ≤ 8 operations            ≤ 8 operations per segment
```

The gate keeps doing exactly what it does. It is simply asked a question it
can answer.

### Segmentation

In `readTranscriptFrom`, which already walks the transcript records. Two rules,
no model call, deterministic:

- **Time gap** — cut when the wall-clock gap between consecutive turns exceeds
  `ingest.segment_gap` (default `20m`). A pause is the cheapest available
  signal that the subject changed: the user went to lunch, came back, started
  something else.
- **Size ceiling** — cut when a segment exceeds `ingest.segment_max_bytes`
  (default `32768`). A continuous four-hour debugging session has no gaps and
  still is not one idea.

Timestamps are available: `transcriptRecord` currently parses only `type` and
`message`, but the JSONL carries `timestamp` on the substantive lines (1489 of
1915 on a sampled real transcript). The struct gains one field. Records
without a timestamp inherit the previous one rather than forcing a cut, since
the missing lines are summaries and meta records, not turns.

A segment shorter than `MinContentLen` is merged into its neighbour rather
than posted — a two-line coda should not become its own source.

Segments never split a turn. Boundaries fall between records, so no source
ever contains half a message.

### Ordering and attribution

Each segment posts with the `occurred_at` of its **first turn**, not the
checkpoint time. This is what makes `occurred_at` mean something for the first
time on this path: today an entire day of work is stamped with the moment the
session closed, and decay reads that timestamp. Segments carry
`external_ref = "<session-id>#<n>"` so the pieces of one session remain
identifiable as such.

Segments post **in order**, and the offset advances only after all of them are
accepted. A partial failure leaves the offset where it was, so the next
checkpoint re-sends the whole range rather than silently dropping the tail.
Re-sending is safe — the gate is what deduplicates — and losing a segment is
not.

### The op budget

Unchanged at 8, and that is the point: 8 per segment instead of 8 per day. A
329 KB session becomes roughly a dozen segments, each free to contribute what
it has. No new setting, no scaling formula.

### Why one source per segment

Every mechanism the extractor already has becomes per-segment for free: the
queue, the gate, the candidate fetch, `MaxOps`, the trace, the 7-day raw
content TTL, and the `queues` event added in rc7. The schema does not change.
The alternative — one source that the extractor splits internally — needs the
gate, the op cap and tracing reworked to loop inside one run, for the sole
benefit of keeping one row per session.

The cost is honest and worth stating: **`sources` row counts rise roughly
tenfold**, and one session no longer maps to one row. `/v1/sources` becomes a
list of segments. Nothing depends on the old ratio, but anyone reading the
table will notice.

## What this does not change

- The gate stays. Defaulting to NOOP is right; most chat is noise. Only the
  unit of the decision moves.
- The surprise threshold stays at 0.93.
- `Stop` is still not a hook. This makes each checkpoint yield more, not
  checkpoints more frequent.
- Nothing about retrieval.

## Settings

Declared in `settings.go` like everything else:

- `ingest.segment_gap` (duration, default `20m`) — `0` disables gap cutting
- `ingest.segment_max_bytes` (int, default `32768`) — `0` disables size cutting

Both zero restores today's behaviour exactly, which is what makes the change
reversible on a real install without a downgrade.

## Testing

No database needed for the segmentation itself, which is where the risk is:

- a transcript with a 30-minute gap cuts there; one with a 5-minute gap does
  not
- a continuous transcript over the byte ceiling cuts at the ceiling, between
  records, never mid-turn
- records missing a timestamp inherit the previous one and do not force a cut
- a trailing fragment below `MinContentLen` merges backwards instead of
  posting
- each segment carries its first turn's `occurred_at`, and they ascend
- both settings at `0` produce exactly one segment for any input — the
  reversibility guarantee, as a test
- a failed mid-sequence post leaves the offset unadvanced

Integration, against a real stack: post a synthetic long session with three
clearly distinct topics and assert three sources arrive, that gating one does
not skip the others, and that total operations exceed what the same content
produces as a single source. That last assertion is the whole design in one
line.

## How we will know it worked

Through [the retrieval eval](./2026-08-18-retrieval-eval-design.md), which is
why that is still first. The corpus there ingests raw sources through
`/v1/ingest`, so it measures this change end-to-end rather than only ranking.

Two numbers, before and after, on the same corpus:

- **extraction yield** — operations per KB ingested, and the count of sources
  gated to zero
- **recall@5 and precision@5** — because yield without precision is just noise,
  and the failure mode of this change is extracting more of what nobody needed

A yield increase with flat recall means we made the corpus louder, not better,
and the honest response is to raise the threshold or back the change out.

## Where the other three designs now stand

- **[Retrieval eval](./2026-08-18-retrieval-eval-design.md)** — unchanged and
  now clearly first. It is what makes this design's claim checkable.
- **[Graph extraction](./2026-08-18-graph-extraction-design.md)** — still
  right, now third. Traversal over 21 rows would have measured nothing; a
  denser corpus is what gives a graph something to connect. Its own value
  proposition improves after this ships.
- **[Working-memory fold](./2026-08-18-working-memory-fold-design.md)** —
  last, and the one to drop if something has to go. It adds a write path that
  nothing currently uses (`working_memory` holds 0 rows), where this design
  improves the write path that carries all real traffic.

## Open questions for review

- Is `20m` the right gap? It is a guess. The eval can answer it by sweeping
  the value, which is an argument for shipping the setting before tuning it.
- Should `pre-compact` be investigated separately? It has never fired on this
  install, which is either correct (no session ever compacted) or a defect.
  Segmentation makes it matter more: compaction is exactly when a long session
  has the most unposted content.
