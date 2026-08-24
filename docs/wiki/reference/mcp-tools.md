# MCP tools reference

The 24 tools Claude can call directly. Mirrors `internal/mcp/server.go`.

The MCP endpoint is served at `/mcp` on the same address as the HTTP API,
wired into `~/.claude.json` by `anamnesia setup`. It carries the same
authentication as the rest of the API: when `server.token` is set, calls need
it.

These are how Claude reads and writes memory **deliberately**, as opposed to
the hooks, which do it automatically. Both paths land in the same store.

---

## Facts

### `anamnesia_facts_upsert`
Upsert a fact in the memory store. Identity is `(user, project, scope, key)`.

An upsert against an existing key supersedes rather than overwrites: the old
row keeps its text, provenance and embedding. See
[Memory model](../memory-model.md#facts).

### `anamnesia_facts_list`
List facts in scope, newest first.

### `anamnesia_facts_forget`
Soft-delete a fact by id. The row stays, with `deleted_at` set.

---

## Experiences

### `anamnesia_experience_record`
Append an experience: trajectory, strategy, or insight.

The `kind` chosen here drives how fast the experience decays. See
[Retrieval](../retrieval.md#decay).

### `anamnesia_experience_supersede`
Mark experience `old_id` as superseded by `new_id`.

### `anamnesia_experience_forget`
Soft-delete an experience by id.

---

## Search and ingest

### `anamnesia_search`
Hybrid (vector + lexical) search across facts, experiences and skills.

Returns an error rather than an empty result when a configured embedder is
down, which is deliberate: an empty result is indistinguishable from "you have
no such memory".

### `anamnesia_ingest`
Push a piece of content into the memory system. The extractor reads it
asynchronously and decides what, if anything, to persist as facts or
experiences.

Use for meeting transcripts and other content that did not come through a
Claude Code session. It goes through the full pipeline, including the surprise
gate, so the answer is often `NOOP`.

---

## Identity and people

### `anamnesia_identity`
Return the user's identity: persona, profile, and a rendered `system_prompt`
block. Dock-on agents call this at boot.

### `anamnesia_people`
List people the user knows, sorted by recent-mention count over the last 90
days.

### `anamnesia_briefing`
Summarise experiences in a time window with "adjacent" items the user might
want to mention. Returns `{summary, highlights[], adjacent[]}`.

---

## Commitments

Off by default. Set `worker.extract_commitments` to have the extractor
populate this ledger automatically; the tools work either way.

### `anamnesia_commitments_record`
Record an open commitment, something owed by or to the user. Status defaults
to `open`.

### `anamnesia_commitments_list`
List commitments. Default sort is open first, then by due date.

### `anamnesia_commitments_resolve`
Mark a commitment done or dropped.

---

## Skills and capabilities

### `anamnesia_skills_register`
Register or update a callable in the skills registry.

### `anamnesia_skills_list`
List skills in scope.

### `anamnesia_capabilities`
List skills and tools registered for this user, freshness-ordered. Use at boot
to discover what you can call.

Skills are served by the lexical channel only, with no vector channel at all.
See [Retrieval](../retrieval.md#the-lexical-channel).

---

## Working memory

### `anamnesia_working_append`
Append an entry to working memory for the given session.

### `anamnesia_working_recall`
Recall the working-memory trail for a session.

Entries carry a TTL and are purged every `worker.forget_every`.

---

## Graph

Entities and edges exist whether or not `graph.extract` is on; that setting
only controls automatic extraction. These tools write to the graph directly.

### `anamnesia_graph_entity`
Upsert a graph entity. Identity is `(scope, kind, name)`.

### `anamnesia_graph_edge`
Create a typed bitemporal edge between two entities.

The edge carries its own validity window, so "who owned this in March" and
"who owns it now" are both answerable.

### `anamnesia_graph_neighbors`
Return entities reachable from `src` via currently-valid edges.

---

## Artifacts

### `anamnesia_artifacts_list`
List the artifacts published in scope, newest first.

Artifacts are written by the `PostToolUse` hook, not by extraction. See
[Memory model](../memory-model.md#artifacts).

---

## Audit

### `anamnesia_audit`
Audit log: tail by default, or per-subject history when `subject` is provided.
