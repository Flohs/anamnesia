# Follow-up: make the eval's numbers trustworthy

Written 2026-08-18 against `feat/retrieval-eval`, from that branch's final
whole-branch review. **Not started.** This is the first work after the eval
merges.

The eval measures retrieval. This is what has to be true before anyone gates
on what it measures.

---

## 1. `waitForDrain` treats failed and skipped sources as drained

**The defect.** `waitForDrain` (`cmd/anamnesia/eval.go`) waits for
`extract_pending` to reach 0, and that count comes from
`extraction_state = 'pending'` (`internal/store/sources.go:133`). But the
worker leaves that state by three doors, not one:

- `MarkExtracted` — the source produced rows
- `MarkFailed` (`internal/jobs/worker.go:231`) — extraction errored
- `MarkSkipped` (`:235`) — the surprise gate returned zero operations

Only the first means the corpus grew. So a run where six of forty sources
errored or were gated out drains "cleanly", and the eval scores a corpus that
is silently short — exactly the half-warm index the drain step exists to
prevent. Worse, it is invisible: nothing in the report says how many sources
actually landed.

**The fix.** After draining, read `GET /v1/stats?user=…&project=…`, which
already returns `sources_by_state` and totals for facts and experiences
(`internal/httpapi/stats.go:87-99`). Put those numbers in the report and print
them. Roughly fifteen lines. A run that ingested 40 sources and extracted from
34 should say so on its face rather than quietly scoring as if it had 40.

Whether a short corpus should *fail* the run or merely be reported is a
judgement to make once we can see the numbers. Report first.

## 2. The variance, and how to find its cause

Two runs over the identical corpus and the same code:

```
run 1:  recall@5 0.700   MRR 0.751   zero-hit 4 of 25
run 2:  recall@5 0.880   MRR 0.823   zero-hit 2 of 25
```

An 0.18 swing against `compareToBaseline`'s 0.02 tolerance. Until this is
explained, `--baseline` cannot gate anything, which the changelog says plainly.

**Three candidates, and item 1 discriminates between them for free.** Once
`sources_by_state` is in the report, two runs show directly whether the corpus
differed:

- **Extraction non-determinism.** Most likely, and more specifically than first
  supposed: the variance is probably in *how many sources produced any rows at
  all* rather than in which facts each produced. The surprise gate's verdict is
  not stable run to run, and `MarkFailed` is invisible today. `sources_by_state`
  shows this immediately.
- **ANN index drift.** A different corpus builds a different HNSW graph. Real,
  but worth a few points, not eighteen.
- **The reranker.** Ranked last. It is deterministic given identical input, so
  it can amplify upstream variance but cannot originate it.

**Do not design the fix before the cause is demonstrated.** Fixing the wrong
layer is worse than the current state, because it would look solved. If
extraction is confirmed, the design change is "ingest once, query the warm
corpus many times" — which separates retrieval measurement from extraction
measurement, and is a different command shape, not a patch.

## 3. Regenerate the baseline, and say which run it is

Two problems with the committed `eval-baseline-rc7.json`:

- It is the **higher** of the two observed runs (recall@5 0.880), so any future
  in-band run reads as a regression.
- It carries no provenance: nothing in the file says which run it was, on what
  corpus, against which model.

Regenerate it once items 1 and 2 land, and record the model, the corpus size,
and the source-state breakdown alongside the metrics.

## 4. Add json tags to `queryScore` / `aggregateScore`

Deferred deliberately from the merge. The artifact currently mixes
`at`/`per_query` with `RecallAt`/`P95MS`, because the outer struct has tags and
the inner ones do not. Adding them makes the *existing* committed baseline
silently unreadable — the fields would unmarshal to zero, recreating the
absent-metric bug the merge just closed.

So this must land in the same change as item 3, never before it.

## 5. The store-level test that was never written

`DeleteUser`'s doc comment now names the commitments exception, and
`deleteEvalScope` works around it. Neither is executable. The test worth having
asserts that `DELETE FROM users` fails with a commitment row present — it needs
no HTTP loop, fits beside the two tests already in `internal/store/store_test.go`,
and is the only executable record of why the workaround exists.

It was swept into "Task 4 needs no tests", which was right for the HTTP loop and
wrong for the invariant underneath it.

## Not in scope

The schema itself. `commitments.user_id` is the only `users(id)` foreign key
without `ON DELETE CASCADE`, and `audit_log.user_id` has no foreign key at all.
That is a latent bug for **any** caller deleting a user, not just the eval, and
it deserves its own decision rather than being fixed as a side effect of this.
