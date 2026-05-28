# Anamnesia as Personal AI Substrate — v1 Design

Date: 2026-05-27
Status: approved, pending implementation
Supersedes the 3-item plan in `docs/session-2026-05-27.md` §"Next session".

## 1. Framing

Anamnesia is not an agent. It is a **kernel** for a personal AI: the memory
substrate that any number of dock-on agents (Claude Code, a phone capture
agent, a weekly-summary cron, a future voice front-end) consume through one
stable contract. The kernel/userland split is the load-bearing principle:

- **Kernel (Anamnesia):** identity, knowledge, relationships, capabilities,
  open-loop state, provenance. One Postgres, one HTTP+MCP surface.
- **Userland (dock-on agents):** calendar sync, email triage, location,
  voice, notification delivery. Each agent ingests *into* the kernel and
  reads *from* it, never the reverse.

Anything that's "a different input source" belongs in a dock-on agent.
Anamnesia stays small.

## 2. What an agent needs to "dock on"

A human personal assistant who knows everything about you operates on
seven kinds of state. Each maps to a substrate primitive:

| Need | Primitive | Status today |
|---|---|---|
| Voice & persona | **Identity** | Buried in `facts`, no boot-side surface |
| "What did I do recently" | **Briefing** | `ListSourcesInWindow` exists; no experience-window query, no synthesis tool |
| "Who do I know" | **People** | `entities`/`edges` exist; no first-class read view |
| "What tools/agents can I lean on" | **Capabilities** | `skills` table exists; no surface for boot-time discovery |
| "What's outstanding (owed by/to me)" | **Commitments** | Not modelled at all |
| "Where did this fact come from" | **Audit** | `audit_log` exists; `anamnesia_audit` tails by scope but cannot filter by subject |
| Proof the contract works | **Reference agent** | None |

## 3. Architecture

No new internal packages required beyond `internal/store` migrations and
HTTP/MCP wiring. Every primitive is a thin surface over existing storage.

```
                  ┌──────────────────────────────────────┐
                  │  Dock-on agents (Claude Code, CLI,   │
                  │  future phone capture, …)            │
                  └────────────┬─────────────────────────┘
                               │ HTTP+MCP (stable contract)
              ┌────────────────┴───────────────────────────┐
              │ /v1/identity   /v1/briefing   /v1/people   │
              │ /v1/capabilities  /v1/commitments          │
              │ /v1/audit?subject=…                        │
              │ /v1/ingest  /v1/experience  /v1/search     │ ← existing
              └────────────────┬───────────────────────────┘
                               │
                  ┌────────────┴─────────────┐
                  │  Postgres (kernel state) │
                  │  facts, experiences,     │
                  │  entities, edges,        │
                  │  skills, commitments,    │
                  │  audit_log               │
                  └──────────────────────────┘
```

### 3.1 Identity primitive

- **Storage:** existing `facts` table. Keys reserved under two namespaces:
  - `user.persona.*` — voice/persona (e.g. `user.persona.system_prompt`,
    `user.persona.tone`, `user.persona.values`).
  - `user.profile.*` — biographical/profile (e.g. `user.profile.name`,
    `user.profile.timezone`, `user.profile.role`).
- **HTTP:** `GET /v1/identity?user=` → `{persona: {key: value, ...},
  profile: {key: value, ...}, system_prompt: "<rendered block>"}`.
- **MCP:** `anamnesia_identity(user?, project?)` returns same shape.
- **SessionStart change:** server-side `/v1/sessions/start` response gains
  a `persona_block` field. The hook prints `## How to respond\n<block>`
  *above* `## Anamnesia memory`.
- **Render rule:** `system_prompt` is the concatenation of
  `user.persona.system_prompt` (if present, verbatim) followed by short
  rendered lines for the other `user.persona.*` keys, then a separator,
  then `user.profile.*` as a `### About me` bullet list. Deterministic, no
  LLM call.

### 3.2 Briefing primitive

- **Storage:** new helper `Store.ListExperiencesInWindow(ctx, scope, since,
  until, opts) ([]Experience, error)` mirroring the existing
  `ListSourcesInWindow`. Optional `opts.Topic` filter does prefix-match on
  `experience.title` *and* embedding cosine if topic provided.
- **HTTP:** `POST /v1/briefing` with body
  `{user?, project?, since, until?, topic?, max_adjacent?}`.
- **Response:** `{window: {since, until}, summary: "<one paragraph>",
  highlights: [{title, why}], adjacent: [{title, why, kind}]}`. The
  adjacent list is the "you might also want to mention…" surface — items
  *near* the window in time or topic but not in it.
- **LLM call:** one call, prompt template owns the two-output format.
  Caches by `(scope, since, until, topic, hash(input_ids))` for 1h.
- **MCP:** `anamnesia_briefing(...)` returns same shape.

### 3.3 People surface

- **Storage:** existing `entities` (where `kind=person`) and `edges`.
- **HTTP:** `GET /v1/people?user=&project=&limit=` →
  `[{id, name, props, recent_mentions: int, last_mentioned_at,
    edges: [{kind, to_name}]}]`.
- **MCP:** `anamnesia_people(user?, project?, limit?)` returns same shape.
- **Recent-mentions count:** join over `experiences.participants` and
  `audit_log.subject_id` for the last 90 days. No new column.

### 3.4 Capabilities surface

- **Storage:** existing `skills` table (already has `use_count`,
  `last_used_at`, `description`, `signature`, `kind`).
- **HTTP:** `GET /v1/capabilities?user=&project=&limit=` →
  `[{name, kind, description, signature, use_count, last_used_at}]`.
  Sort: `last_used_at DESC NULLS LAST, use_count DESC`. No new column.
- **MCP:** `anamnesia_capabilities(user?, project?, limit?)` returns
  same shape.
- The existing `anamnesia_skills_list` stays — it's the write-shaped
  view (name-ordered). `anamnesia_capabilities` is the boot-shaped view
  (freshness-ordered, signature included for tool discovery).

### 3.5 Commitments domain

The only primitive that needs new storage. Commitments are first-class
because multi-agent coherence depends on a single ledger.

- **Migration `0007_commitments.sql`:**

  ```sql
  CREATE TABLE commitments (
      id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
      user_id     uuid NOT NULL REFERENCES users(id),
      project_id  uuid REFERENCES projects(id),
      owner       text NOT NULL,            -- party owing (free string;
                                            -- "user" / "<person name>")
      beneficiary text NOT NULL,            -- party owed
      body        text NOT NULL,
      due_at      timestamptz,
      status      text NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open','done','dropped')),
      source_id   uuid REFERENCES sources(id),
      created_at  timestamptz NOT NULL DEFAULT now(),
      updated_at  timestamptz NOT NULL DEFAULT now()
  );
  CREATE INDEX commitments_user_status_due
      ON commitments (user_id, status, due_at);
  CREATE INDEX commitments_project_status
      ON commitments (project_id, status)
      WHERE project_id IS NOT NULL;
  ```

- **Store API:** `RecordCommitment`, `ListCommitments(scope, status,
  limit)`, `ResolveCommitment(id, status)`. No update of body — record
  a new one + drop the old to preserve audit lineage.
- **HTTP:**
  - `POST /v1/commitments` → body matches `Commitment`; returns id.
  - `GET /v1/commitments?user=&project=&status=open&limit=` → list.
  - `POST /v1/commitments/{id}/resolve` body `{status: done|dropped}`.
- **MCP:** `anamnesia_commitments_record`, `_list`, `_resolve`.
- **Extractor opt-in:** extractor prompt gains a fifth op,
  `ADD_COMMITMENT {owner, beneficiary, body, due_at?}`. NOOP remains
  default. Off by behavior flag `ANAMNESIA_EXTRACT_COMMITMENTS=true` so
  benchmark runs aren't perturbed.

### 3.6 Audit read surface

Extend, don't replace.

- **HTTP:** `GET /v1/audit?subject=<kind>:<uuid>&limit=` for
  per-subject history; existing `?user=&project=` tail mode kept.
- **MCP:** `anamnesia_audit` gains optional `subject` argument
  (string `"fact:<uuid>"`, `"experience:<uuid>"`, `"entity:<uuid>"`,
  `"commitment:<uuid>"`).
- **Store:** new `AuditForSubject(ctx, kind, id, limit)` query.

### 3.7 Dock-on reference agent

- **Location:** `examples/agents/cli-companion/` (separate Go module so
  it can't accidentally import Anamnesia internals).
- **Dependencies:** Anthropic Agent SDK + the MCP client from
  `mark3labs/mcp-go`.
- **Behavior on boot:**
  1. Connect to `http://localhost:8181/mcp`.
  2. Call `anamnesia_identity` → render as system prompt.
  3. Call `anamnesia_capabilities` → log "<N skills available>".
- **Behavior per turn:**
  1. Read user prompt.
  2. Call `anamnesia_search(prompt)` for top-K context.
  3. Call `anamnesia_people` if prompt mentions a person name.
  4. Compose LLM call (Claude via Anthropic SDK) with persona + context.
  5. If response contains a future-tense commitment, call
     `anamnesia_commitments_record`.
- **Behavior on exit:**
  1. Call `anamnesia_ingest` with the full transcript so the kernel can
     extract.
- **Acceptance:** must compile and run without importing anything from
  `github.com/flohs/anamnesia-open-source/internal/...`. CI gate enforces
  this via `go list -deps`.

## 4. Build order

Sequence is locked. Each item is independently shippable and adds a
working surface.

| # | Item | Effort | Depends on |
|---|---|---|---|
| 1 | Identity primitive | 1d | — |
| 2 | Briefing primitive (+ `ListExperiencesInWindow`) | 1d | — |
| 3 | People surface | 0.5d | — |
| 4 | Capabilities surface | 0.5d | — |
| 5 | Commitments domain (migration + CRUD + MCP) | 1.5d | — |
| 6 | Audit read surface (subject filter) | 0.5d | — |
| 7 | Dock-on reference agent | 1.5d | 1–6 |

Total: ~6.5d wall-clock. Items 1–6 can be parallelised; item 7 is the
integration test.

## 5. Success criteria

Concrete, testable:

1. A fresh dock-on agent adopts the user's voice with **one** API call
   (`anamnesia_identity` returns a non-empty `system_prompt`).
2. The query "what did I do this week in `<project>`" succeeds with
   **one** API call (`anamnesia_briefing` returns a `summary` and
   non-empty `adjacent`).
3. The CLI reference agent boots, holds persona, retrieves context, and
   records at least one commitment in a scripted scenario — without
   importing `internal/...`.
4. `anamnesia_audit(subject="fact:<id>")` returns the chronological
   history of changes for a fact written and updated via different
   agents (i.e. provenance survives multi-agent writes).

## 6. What's explicitly NOT in v1

Listed so they don't get re-litigated:

- **Push / subscriptions** — agents poll; no webhooks or change-streams.
  Track for v2 once we know what events agents actually want.
- **Cross-device sync** — local laptop only. Sync is a separate problem.
- **Wiki/vault renderer** — already specced in `docs/wiki-layer-plan.md`;
  that's a parallel v1, not part of substrate v1.
- **Calendar / email / location ingestion** — these are dock-on agents,
  not kernel. Build the first one (CLI companion); others follow the
  same shape.
- **Per-agent auth / scoping** — `ANAMNESIA_SERVER_TOKEN` is sufficient
  for single-user-local. Trust model lives in front of the API, not in it.
- **Conflict-resolution model beyond last-writer-wins** — facts already
  carry `trust`; multi-agent merge policy is a v2 concern.
- **Commitment reminders / notifications** — commitments are *stored*
  in v1; *delivery* is a v2 dock-on agent's job.

## 7. Risks and open questions

- **Persona ranking when SessionStart hits its `MaxFacts=50` cap.**
  Today persona facts compete with everything else. Fix: `identity`
  uses key-prefix queries, not the ranked pool. SessionStart pulls
  persona separately and *appends* the ranked memory block.
- **Briefing latency.** One LLM call per request is fine for an opt-in
  surface; if it becomes hot we cache per `(scope, window, topic)` for
  1h and invalidate on new experience writes in that window.
- **Commitment extraction quality.** Off by default; turn on per-user
  via `user.persona.extract_commitments=true` once we trust the prompt.
- **People surface scale.** Recent-mentions join is N*M-ish at high
  cardinality. Acceptable today (single user, <10k experiences);
  materialise via a daily roll-up if it gets slow.

## 8. Mapping to the prior plan

The 3-item plan from `docs/session-2026-05-27.md` collapses into this
spec as follows:

- "Persona primitive" → §3.1 Identity
- "Proactive briefing MCP tool" → §3.2 Briefing
- "Agent SDK reference app" → §3.7 Reference agent

The other 4 items (People, Capabilities, Commitments, Audit) are the
delta this design adds to make the substrate genuinely sufficient for
"a personal assistant who knows everything."
