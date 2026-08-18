# Handover 2: what the console found in rc5

Written 2026-08-18 against `v0.1.0-rc5` (commit d96c144), from running the
console against a live install.

Both items from [handover 1](./2026-08-18-console-handover.md) landed and the
console consumes them. `step_timings` let it delete the 15-requests-per-render
workaround entirely: the activity feed now draws every bar from the summary
frame, with zero follow-up requests, verified in the browser.

Two new items below, then the status of what is still open.

---

## 1. The stream never emits a `queues` event

**Priority: medium.** There is a working client-side answer, so nothing is
broken. But the contract and the implementation disagree, and that disagreement
silently froze a widget for as long as a page stayed open.

### What happened

The console showed `1` under "awaiting embedding". The embedding completed, the
worker lane updated correctly within a second, and the tile kept showing `1`
indefinitely.

### Why

Queue depth reaches the console in the `snapshot` frame and never again. The
recorder can emit three event kinds, `loops`, `step` and `trace`; there is no
`queues` kind. Observed on a live stream: 24 `loops` events in 12 seconds, and
no `queues` event, ever, because none can be constructed.

The stream contract in the original spec lists a `queues` event, so this is a
gap rather than a decision.

### The change

Publish queue depth when it changes. Depth moves on exactly three occasions: a
source arrives, the extractor finishes one, and the embed worker backfills a
batch.

The cheap shape is to recompute only after a tick that actually did work,
rather than on every tick. `extract` runs once a second on a default install,
and `QueuePending` is two `COUNT`s, so an unconditional query per tick would
add a steady query-per-second for nothing while the machine is idle:

```go
// internal/jobs/worker.go, after a tick that reported work
if result != "" && !isIdle(result) {
    extract, embed, err := w.Store.QueuePending(ctx, userID)
    if err == nil {
        w.Activity.PublishQueues(extract, embed)
    }
}
```

with a matching `queues` event kind in `internal/activity`, serialised the way
the snapshot already serialises it:

```json
event: queues
data: {"extract_pending":0,"embed_pending":2}
```

### What the console does meanwhile

It polls `/v1/queue/pending` every five seconds, which is a two-field response.
When the event starts arriving, the poll becomes redundant and will be removed.
No coordination needed: the console prefers whichever source is fresher.

### Verifying

Watch the stream while an ingest is extracted:

```bash
curl -sN localhost:8181/v1/activity/stream | grep --line-buffered '^event:'
```

A `queues` frame should appear as the depth changes, and not otherwise.

### Not a defect, noted so nobody 'fixes' it

`queues` is `omitempty` on the snapshot and absent when the server has no
database (`internal/httpapi/activity.go:37`). The console handles its absence.

---

## 2. `/v1/hooks` ignores `limit`

**Priority: low.**

```bash
curl -s 'localhost:8181/v1/hooks?limit=5'  | jq '.items | length'   # 88
curl -s 'localhost:8181/v1/hooks?limit=100' | jq '.items | length'  # 88
```

The whole of `hooks.log` comes back regardless. At 88 entries that is
harmless; on an install that has been running for months it is a large
response for a panel that wants the last handful. Every other list endpoint
honours `limit`, so this is an inconsistency as much as a size problem.

The console caps at 15 client-side and says how many it is not showing, so
this is not urgent.

---

## Still open

**Experience titles: unverified, through no fault of the change.** The prompt
now asks for one, but no extraction has run since rc5 shipped. All four
experiences under `default` predate the build (latest ingest 11:29 UTC, rc5
built 11:44 UTC) and all four are untitled at abstraction 0. The next
extraction will settle it. The console falls back to the body's first sentence
in the meantime, so an untitled row reads acceptably either way.

**`audit_log` has no writers**, as before. `/v1/audit` returns an empty list
and the console does not consume it. Still the only route to per-row
provenance and to distinguishing what the extractor inferred from what Claude
wrote deliberately through MCP.

---

## Console-side changes this prompted

Recorded here only so the two sides stay legible to each other; nothing to do.

- Queue depth polled from `/v1/queue/pending` rather than trusted from the
  stream.
- "Traces held" now counts the client's own list. It had been reading
  `recorder.held`, which arrives once in the snapshot and never moves.
- Uptime is computed from `server.started_at` rather than read from
  `uptime_ms`, which is measured at connect and would otherwise still read
  "3m uptime" hours later. The same frozen value gated the "traces from before
  the last restart are gone" notice, which would have stayed on screen
  indefinitely.
