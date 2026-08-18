# Handover: two changes the console needs from the binary

Written 2026-08-18 against `v0.1.0-rc4` (commit d315ff4), from integrating the
Anamnesia Console against the live read API.

Everything else the console needs, rc4 already serves correctly. These are the
two remaining items, in priority order. The first removes code from the
console; the second improves what gets stored.

---

## 1. Put step timings on trace summaries

**Priority: high.** It deletes a workaround rather than adding a feature.

### The problem

`/v1/activity` returns trace summaries carrying `step_count` but no durations.
The console's main screen draws each trace as a segmented bar sized by how long
each step took, so a glance answers "where did the time go". With only a count,
it cannot.

The console currently works around this by fetching the 15 newest traces in
full, purely to read numbers the summary could have carried. That is up to 15
extra requests per feed render, each pulling the complete step detail
(conversation excerpts, model responses) to use two fields from it.

### Where it is lost

`internal/activity/activity.go:385`

```go
func (t *Trace) summary() *Trace {
	c := *t
	c.rec = nil
	c.Steps = nil          // <- timings discarded here
	c.StepCount = len(t.Steps)
	return &c
}
```

`internal/httpapi/activity.go:267`, `viewTrace(t, withSteps=false)`, then emits
`step_count` alone. The comment above it is right and worth keeping:

> a ring of 200 traces with every step inlined is a payload nobody asked for

Step timings respect that. They carry no `Detail`, no `Summary`, no `Err`.

### The change

Keep names and durations in the summary, and serialise them.

```go
// activity.go — a step reduced to what a bar needs
type StepTiming struct {
	Name     string
	Duration time.Duration
}

func (t *Trace) summary() *Trace {
	c := *t
	c.rec = nil
	c.Steps = nil
	c.StepCount = len(t.Steps)
	c.StepTimings = make([]StepTiming, len(t.Steps))
	for i, s := range t.Steps {
		c.StepTimings[i] = StepTiming{Name: s.Name, Duration: s.Duration}
	}
	return &c
}
```

```go
// httpapi/activity.go
type stepTimingView struct {
	Name       string `json:"name"`
	DurationMS int64  `json:"duration_ms"`
	// Optional. Only `model` and `verdict` are read, to label a segment
	// "llm · gpt-4o-mini" or "gate · skip". Omit it and the segment is
	// labelled with the step name alone, which is fine.
	Detail map[string]any `json:"detail,omitempty"`
}

type traceView struct {
	// … existing fields …
	StepTimings []stepTimingView `json:"step_timings,omitempty"`
}
```

### Wire format

```json
{
  "id": "74aac918-…",
  "kind": "retrieve",
  "status": "ok",
  "duration_ms": 730,
  "step_count": 6,
  "step_timings": [
    { "name": "query",   "duration_ms": 0 },
    { "name": "vector",  "duration_ms": 348 },
    { "name": "lexical", "duration_ms": 0 },
    { "name": "fuse",    "duration_ms": 0 },
    { "name": "rerank",  "duration_ms": 0 },
    { "name": "result",  "duration_ms": 381 }
  ]
}
```

Apply it to the `snapshot` and `trace` SSE frames as well as `/v1/activity`,
since the console renders the feed from whichever arrives first.

### Cost

A full ring of 200 traces at 6 steps averages roughly 48 kB of additional JSON,
against the current alternative of the console pulling 15 complete traces.

### Verifying

```bash
curl -s localhost:8181/v1/activity | jq '.traces[0].step_timings'
```

Every trace in the list should carry one entry per step, in execution order,
with durations summing to roughly `duration_ms`. The console needs no change
to consume it: `step_timings` is already in its types and is preferred over the
fallback whenever present.

---

## 2. Have the extractor name what it stores

**Priority: medium.** Nothing is broken; the stored data is just less useful
than it could be.

### The problem

Experiences frequently arrive with no `title`. On the install this was written
against, both stored experiences are untitled, so a list of memories reads as a
column of body text:

> "Implemented a systematic approach to debugging within the emulator, ensuring
> all changes maintain fidelity to the controller API and improving overall
> functionality."

The console falls back to the body's first sentence, which is a guess about
something the model was already in a position to state plainly. The cost is not
only cosmetic: `experiences.title` feeds the tsvector used by lexical
retrieval, so an untitled experience is a weaker search target than it should
be.

### Where

The operation schema at `internal/extract/extract.go:696` already permits a
title:

```go
"title": {"type": "string"},
```

The instructions never ask for one. `extract.go:729`:

> ADD_EXPERIENCE: a noteworthy event, trajectory, strategy, or insight worth
> remembering as a narrative. Use kind=case|strategy|hybrid. Provide a 1-2
> sentence body.

and again at `:758` for the multi-claim variant. Both ask for a body and stop.

### The change

Ask for the title in both instruction sites, and make it required in the
schema for `ADD_EXPERIENCE` so a model that skips it is retried rather than
silently accepted:

> ADD_EXPERIENCE: … Provide `title`, a short noun phrase naming what this is
> (under 60 characters, no trailing full stop), and `body`, 1-2 sentences.

Worth stating explicitly in the prompt that the title should name the
conclusion rather than the activity: "Hook ordering is guaranteed by the
client" rather than "Discussion about hooks". The first is retrievable, the
second is not.

### Verifying

```bash
anamnesia doctor --deep
curl -s 'localhost:8181/v1/experiences?limit=5' | jq '.items[].title'
```

No `null` entries, and each title readable on its own without the body.

---

## Not defects, no action needed

Checked while integrating, and correct as they stand:

- `err` on a step and `valid_to` on a fact are `omitempty` and rightly absent
  when empty.
- `/v1/stats/activity` returning only days that had activity is the efficient
  shape. The console fills the quiet days itself.
- `scope` carrying `user_id` and `project_id` rather than names is right. The
  console resolves them once against `/v1/projects` and `/v1/users`.
- Facts wrapping scalars as `{"v": 8}` is fine; the console unwraps it.

## Known gap, deliberately deferred

`audit_log` has no writers (`WriteAudit` has zero callers), so `/v1/audit`
returns an empty list. The console does not consume it. Activating it would
enable per-row provenance, `?subject=fact:<uuid>`, and would be the only record
distinguishing a fact the extractor inferred from one Claude wrote deliberately
through MCP. Worth doing eventually; nothing depends on it today.
