# HTTP API reference

Every route the server serves. Mirrors the mux in
`internal/httpapi/server.go`.

Base URL is `server.addr`, `http://127.0.0.1:8181` by default.

## Authentication

Every route except `/v1/health` is protected. When `server.token` is set,
send it:

```
Authorization: Bearer <token>
```

`/v1/health` stays unauthenticated so a load balancer can probe it.

With `server.addr` on loopback and no token set, nothing outside your machine
can reach the API. Binding anything else without a token exposes your memory.
See [SECURITY.md](../../../SECURITY.md).

---

## Health

### `/v1/health`
Unauthenticated. Checks the database connection, the schema version, the
embedding width and the ANN indexes, and reports which one failed.

**It can fail, and that is the point.** A health check that cannot fail turns
a broken install into a green light.

---

## Core

### `/v1/sessions/start`
What `SessionStart` calls. Returns the facts and recent experiences to load
into a new session.

### `/v1/retrieve`
What `UserPromptSubmit` calls. Hybrid retrieval for a prompt.

Returns an **error** when a configured embedder is down, never an empty result
set. A credit outage once had this route answer `200` with no hits for a user
holding hundreds of fully-embedded facts, which is indistinguishable from
having no such memory.

### `/v1/ingest`
Push content into the memory system. Lands as a `sources` row for the
extractor to drain.

### `/v1/queue/pending`
How many sources are awaiting extraction. What `doctor` reads for its queue
check.

### `/v1/experience`
Record an experience directly, without going through extraction.

---

## Identity

### `/v1/identity`
The user's persona, profile, and a rendered system-prompt block. What dock-on
agents call at boot.

### `/v1/people`
People the user knows, by recent-mention count over the last 90 days.

### `/v1/briefing`
Experiences in a time window, summarised, with adjacent items.

---

## Capabilities and commitments

### `/v1/capabilities`
Registered skills and tools for this user, freshness-ordered.

### `/v1/commitments`
The commitments ledger. Open first, then by due date.

### `/v1/commitments/resolve`
Mark a commitment done or dropped.

---

## Artifacts

### `/v1/artifacts`
Artifacts published in scope, newest first.

---

## Audit

### `/v1/audit`
Audit log: tail by default, per-subject history when `subject` is given.

---

## Browse

Seven domains, each with a list route and a detail route:

```
GET /v1/facts          GET /v1/facts/{id}
GET /v1/experiences    GET /v1/experiences/{id}
GET /v1/skills         GET /v1/skills/{id}
GET /v1/entities       GET /v1/entities/{id}
GET /v1/edges          GET /v1/edges/{id}
GET /v1/sources        GET /v1/sources/{id}
GET /v1/working        GET /v1/working/{id}
```

List responses are cursor-paginated: `{items, next_cursor}`. A null cursor
means the end.

These are the read surface a console or dashboard consumes.

---

## Activity

Traces of what the server has been doing. Requires `activity.enabled`, which
is on by default; with it off these routes 404.

Traces live in memory only, so a restart clears them. `activity.traces` (200)
is how many are kept.

### `GET /v1/activity`
Recent traces, with step timings on each summary.

### `GET /v1/activity/stream`
The same as a stream.

### `GET /v1/activity/{id}`
One trace in full, including every step and its reason in plain words. This is
where the surprise gate's verdict is readable:

```
gate   Kept: the nearest memory scores 0.71, below the 0.93 threshold
gate   Skipped: an existing memory already covers this
```

---

## Introspection

### `GET /v1/config`
The resolved configuration, with secrets masked.

### `GET /v1/hooks`
What hooks are wired and how they have been doing. The API side of what
`doctor` reports.

### `GET /v1/stats`
Row counts per domain.

### `GET /v1/stats/activity`
Activity bucketed over time.

### `GET /v1/projects`
Every project slug with memories.

### `GET /v1/users`
Every user with memories.

### `GET /v1/embedding-map`
A projection of the embedding space, for visualisation.

---

## MCP

### `/mcp` and `/mcp/`
The MCP endpoint, carrying the same authentication as everything else. Wired
into `~/.claude.json` by `anamnesia setup`.

See the [MCP tools reference](mcp-tools.md) for the 24 tools it exposes.
