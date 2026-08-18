# Deferred: mid-session checkpoints

Written 2026-08-18 against `v0.1.0-rc7`. **Deferred deliberately — recorded so
the reasoning is not lost, not scheduled.**

Do not act on this without an explicit decision. It touches an invariant.

---

## The concern

With compaction avoided (see the resolution in [segmented
ingest](./2026-08-18-segmented-ingest-design.md)), `SessionEnd` is the only
path by which memory is written. `PreCompact` has never fired and, given how
this operator works, is not expected to.

Two consequences:

1. **Everything a session learned arrives in one lump at the end.** Segmented
   ingest addresses this — it is why that design matters more here than on an
   install that compacts.
2. **A session that does not end cleanly contributes nothing.** The hook log
   shows 8 `session-start` against 6 `session-end`. Some of that gap is
   sessions still open; it is not known how much. A crash, a timeout, or a
   terminal closed without exiting loses the whole session's memory.

Consequence 2 is not addressed by anything currently designed.

## Why this is delicate

The obvious fix is a more frequent checkpoint, and the obvious mechanism is
the `Stop` hook, which was **deliberately removed**. `CLAUDE.md` records it as
an invariant:

> (`Stop` must not be used for this: it fires after every assistant turn,
> which made ingest quadratic in session length.)

That reasoning was correct when written. It has since partly expired: the
per-session byte offset in `~/.anamnesia/offsets/` means a checkpoint sends
only what was added since the last one, so N checkpoints over a session send
the transcript once in total rather than N times. The quadratic blowup is
structurally impossible now.

What remains is a different and weaker objection: **cost and noise per
extraction**. Every checkpoint is a source, a gate evaluation, and possibly an
LLM extraction call. Firing on every assistant turn would mean many small
extractions over content that is mostly mid-thought, where the interesting
conclusion has not been reached yet. That is a real objection, just not the
one the invariant records.

## Options, if this is ever picked up

- **A periodic checkpoint** on a timer rather than per turn — every N minutes
  of session time, independent of turn count. Keeps the offset benefit,
  bounds the extraction rate, and does not reinstate `Stop`.
- **A size-triggered checkpoint** — post when unposted transcript bytes exceed
  a threshold. Composes naturally with segmented ingest, which already counts
  bytes to decide segment boundaries.
- **Leave it.** Accept that an unclean exit loses a session. This is the
  status quo and may well be correct: the loss is bounded by one session, and
  the alternative spends model budget on incomplete thoughts.

## What would make this decidable

The eval, again. A periodic checkpoint changes what enters the corpus, so its
effect on recall and precision is measurable the same way segmented ingest's
is. Until then any choice here is preference, not evidence.

## Prerequisite

Do not start this before [segmented
ingest](./2026-08-18-segmented-ingest-design.md) ships. Both change checkpoint
behaviour, and landing them together would make it impossible to attribute a
change in the numbers to either one.
