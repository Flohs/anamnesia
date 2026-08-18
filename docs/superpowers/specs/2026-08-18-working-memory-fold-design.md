# Design: finish the working-memory lifecycle

Written 2026-08-18 against `v0.1.0-rc7` (commit f9bb1be).

Smallest of the four designs, and independent of the others.

**Reordered 2026-08-18: this is now last, and it is the one to drop if
something has to go.** It adds a write path nothing currently uses
(`working_memory` holds 0 rows), where [segmented
ingest](./2026-08-18-segmented-ingest-design.md) improves the write path that
carries all real traffic.

---

## The problem

`working_memory` is documented as `append · recall · fold → Experience`. Two of
those three work.

`Store.FoldWorking` is implemented (`internal/store/working.go:92`): it marks
every unfolded entry for a `(scope, session)` as belonging to a given
experience and returns the count. The `folded_into` column exists, and there is
a partial index built specifically for the unfolded case:

```sql
CREATE INDEX working_memory_expires ON working_memory (expires_at)
    WHERE folded_into IS NULL;
```

**`FoldWorking` has zero callers.** No MCP tool, no HTTP route, no hook. So an
entry can be appended and recalled, and then it expires and is gone. The
in-session scratchpad has no path into long-term memory, which is the one thing
that would make it worth writing to.

That it currently holds 0 rows on a live install is consistent with this: there
is little reason to append to a store that forgets unconditionally.

## Design

Both triggers, because they answer different questions.

### Track A — `anamnesia_working_fold`, on intent

An MCP tool, alongside the existing `anamnesia_working_append` and
`anamnesia_working_recall`:

```
anamnesia_working_fold
  session_id  required
  title       required — names the conclusion, not the activity
  body        optional — when absent, the server distils one from the entries
  kind        optional — case | strategy | hybrid, default case
```

It writes one experience, then calls `FoldWorking` with that experience's id,
and returns the count folded. This is the path for "this session's scratch work
is worth keeping", decided by the only participant who knows whether it was.

When `body` is absent the server distils it with one LLM call over the entries,
reusing the consolidation distil prompt shape rather than inventing a second
one. When `body` is present, no model call happens at all — a caller that knows
what it wants should not pay for a model to agree.

### Track B — the SessionEnd sweep

`SessionEnd` already checkpoints the transcript. It gains a second step: if the
session has unfolded working entries, fold them.

The bar has to be higher than "entries exist", or every session with a
scratchpad writes an experience whether or not it earned one. Two gates:

- `working.fold_min_entries` (int, default 3) — below this, let them expire
- the same surprise gate ingest uses — if an existing memory already covers the
  folded body, `NOOP` and let them expire

Without the second gate this is a machine for generating near-duplicate
experiences once per session, which is precisely the memory bloat the project
already decays against.

New settings, declared in `settings.go`:

- `working.fold_on_session_end` (bool, default **true**)
- `working.fold_min_entries` (int, default 3)

Default on: entries only exist because something deliberately appended them, and
silently discarding deliberate writes is the worse failure. An install that
disagrees sets the flag.

### Ordering within SessionEnd

The fold runs **after** the transcript checkpoint is queued, and never blocks
it. The checkpoint is the load-bearing half of `SessionEnd`; a fold that fails
must not cost the session its transcript. As with every hook, exit 0 whatever
happens and record the outcome in `hooks.log`.

## What the folded experience looks like

```
kind         case (default) or as given
title        from the tool, or distilled
body         from the tool, or distilled from the entries
abstraction  0 — it is a record of one session, not a consolidation
occurred_at  the earliest entry's created_at, not now()
provenance   {"folded_from_session": "<uuid>", "entries": 7}
meta         {"folded": true}
```

`occurred_at` matters: an experience about work done across an afternoon is not
an event that happened at the moment the session closed, and decay reads that
timestamp.

`abstraction 0` matters too — the consolidation worker clusters abstraction-0
rows into abstraction-1 insights, so a folded session becomes eligible for
consolidation like any other experience rather than sitting outside the
hierarchy.

## Testing

DB-backed (skipped without `ANAMNESIA_TEST_DATABASE_URL`):

- folding writes one experience and marks exactly that session's unfolded
  entries, leaving another session's alone
- folding twice folds nothing the second time — `folded_into IS NULL` is the
  guard, and this is the idempotence that stops a retried hook duplicating an
  experience
- entries below `fold_min_entries` are left to expire
- the folded experience carries the earliest entry's `occurred_at`

No-DB:

- the MCP tool with an explicit `body` makes no LLM call
- `fold_on_session_end = false` leaves the sweep out of the hook path

## Rollout

1. MCP tool, since it is the path with a caller that can decide.
2. SessionEnd sweep behind its flag.
3. Watch `working_memory` on a real install for a week. If nothing appends,
   this design solved a problem nobody had, and that is worth knowing before
   building more on top of it.

## Open question for review

Whether Track B should fold into **one** experience per session or let the
distil step split a long session into several. One is simpler and matches the
"one session, one record" intuition; several would suit a session that did
three unrelated things. This design assumes one.
