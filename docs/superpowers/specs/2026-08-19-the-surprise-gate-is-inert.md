# Finding: the surprise gate is close to inert, and one field hides it

Written 2026-08-19, from measurements taken while proving segmented ingest.
**Not a plan. A set of findings with numbers, and what they imply.**

Nothing here was caused by segmented ingest; that work only made it visible.

---

## 1. The gate never skips anything

Across 50+ real ingests against a live stack with a real model and a real
reranker — two full transcript runs plus four isolated probes — the surprise
gate returned `verdict: keep` **every single time**.

Probes against a controlled scope already holding the target content:

| content sent | gate score | threshold |
|---|---:|---:|
| a paraphrase of a stored fact | 0.41 | 0.93 |
| a near-verbatim restatement | 0.45 | 0.93 |
| a byte-identical copy of already-extracted text | **0.60** | 0.93 |

A literal duplicate of text already in the store scores 0.60. The threshold is
0.93. Whatever `extract.surprise_threshold` was tuned against, it is not this
content, and at its shipped default the gate cannot fire.

This matters because the gate is the project's stated defence against memory
bloat — "Default is NOOP, because most chat is noise". That defence is not
currently operating. What has been doing the filtering instead is the
extractor's own judgement inside the LLM call, plus the 8-operation cap.

## 2. One temporal marker bypasses the gate entirely

`hasTemporalMarker` (`internal/extract/extract.go:197`) skips the gate for
content carrying an explicit temporal marker, on the reasoning that the user is
telling the agent something just changed. The check runs over the **whole
source**.

In a 20 KB checkpoint, one occurrence of "last year" anywhere in it was enough
to bypass the gate for the entire body. A real session of any length will
almost always contain some temporal phrase, so for whole-session checkpoints
this bypass is closer to the rule than the exception.

Segmented ingest narrows the blast radius — the bypass now applies per segment
rather than per session — but does not address the check itself.

## 3. `extraction_state = 'skipped'` conflates two different things

A source lands in `skipped` when **either**:

- the gate skipped it (`MarkSkipped` after a skip verdict), or
- the gate kept it and the LLM returned zero operations

There is no way to tell those apart from the row. The distinction lives only in
the in-memory activity trace, which a restart discards.

This is not academic. The design document for segmented ingest asserted that a
329 KB production source producing zero memories was a gate skip, on the
strength of this field. Measurement showed the gate was almost certainly not
involved. **The field was read exactly the way anyone would read it, and it
gave the wrong answer.**

Cheapest fix: distinct states, `gated` and `no-ops`, or a nullable
`skip_reason` column. Either makes the field answer the question people
actually ask it.

## 4. The gate may not compare against the best available match

Observed on the byte-identical-duplicate probe: the gate's own `K=1` search
returned a top hit scoring **0.60** against a fact, while a separate `K=5`
candidate fetch for the same content surfaced an experience scoring **0.89**.

The gate asks for one nearest neighbour and judges against it. If a K=5 search
finds a materially closer match than the K=1 search, the gate is judging
against the wrong row. That would depress every gate score and is a candidate
explanation for finding 1 — though not the only one, since 0.89 is still below
0.93.

Not investigated further; it belongs to whoever owns retrieval, and it needs
its own before/after rather than being fixed as a side effect.

## What follows from this

These are ordered by how much they change what the system does, not by effort.

1. **Decide what the gate is for.** At 0.93 it is inert. Either lower the
   threshold until it fires on genuine duplicates — the 0.60 measurement above
   is a starting point for what "duplicate" actually scores — or accept that
   filtering happens in the extraction call and retire the gate rather than
   keeping a defence that does not defend. Do not tune it by guess: the eval
   (`anamnesia eval`) now measures retrieval, and a gate change is exactly the
   kind of thing it exists to judge.

2. **Split `extraction_state`.** Cheap, and it stops the next person reaching
   the wrong conclusion from the same field.

3. **Narrow `hasTemporalMarker`.** Per segment is better than per session,
   which segmented ingest already achieves. Whether a single marker should
   bypass a whole segment is still worth asking.

4. **Chase the K=1/K=5 discrepancy.** Smallest scope, and it may partly
   explain finding 1.

## What this does not change

Segmented ingest stands on its own measurement: 0 of 5 novel subjects survived
an unsegmented 32.5 KB transcript, 5 of 5 survived segmented, reproduced twice
on fresh scopes. That result is about the operation budget, not the gate, and
none of the above alters it.
