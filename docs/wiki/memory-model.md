# Memory model

Anamnesia does not store your conversations. It stores what a model judged
worth keeping, in six typed domains, and throws the transcript away after
seven days.

Typed storage is the bet the whole project rests on. A pile of text can only
answer "which paragraphs mention this word". Typed rows can answer "what is
this set to now", "what was it before", "what did we decide and why", and
"what is connected to what", because the shape of the question matches the
shape of the storage.

## The six domains

| Domain | Holds | Identity |
|---|---|---|
| `facts` | Keyed claims: preferences, project configuration | `(scope, fact_scope, key)` |
| `experiences` | Time-stamped narratives of what happened | uuid |
| `skills` | A registry of callable things | `(scope, name)` |
| `working_memory` | In-session entries that expire | `(session, position)` |
| `entities` + `edges` | A bitemporal graph | `(scope, kind, name)` |
| `artifacts` | Pages Claude Code published, as pointers | `(user, artifact_uuid)` |

A seventh table, `commitments`, records open obligations. It is off by default
(`worker.extract_commitments`) because most installs do not want it.

## Scope

Every row carries a `Scope`: a user id and an optional project id. Memory
partitions by user automatically, which is what lets a small team share one
server by setting distinct `identity.user` values.

Facts carry a second axis, `fact_scope`, which is one of:

- **`user`**, true of you wherever you are: "prefers nvim"
- **`project`**, true of one repository: "deploys with docker compose"
- **`environment`**, true of this machine

That second axis is why `editor.preference` set at the user level does not
collide with a project-level fact of the same key. Identity for a fact is the
triple `(scope, fact_scope, key)`, not the key alone.

## Facts

A fact is a key and a JSON value, with provenance and a validity window.

```
key        user.editor.preference
value      {"v": "nvim"}
fact_scope user
trust      0.9
source_id  the row this was extracted from
valid_from 2026-08-01T09:14:22Z
valid_to   null
```

**Facts have history.** Changing a value does not overwrite the old one. The
previous row gets `valid_to`, `superseded_by` and `invalidated_at` set, and
keeps its own text, provenance and embedding. So "what did I think last month"
is answerable, and "what do I think now" is the default.

Old values stay out of your prompts unless you ask for them, with
`include_history` on a search. This matters more than it sounds: a superseded
value competing with the current one in retrieval is worse than not having the
history at all.

Facts are soft-deleted (`deleted_at`), never removed.

## Experiences

An experience is a narrative with a timestamp. It is what facts cannot be: the
record of something happening, rather than something being the case.

```
kind         case | strategy | hybrid
abstraction  0 = raw episode, higher = distilled
title        "debugging the RRF fusion"
body         free text
outcome      success | failure | partial
occurred_at  world time the thing happened
ingested_at  when we learned about it
participants ["floh"]
topic        "retrieval"
parent_id    the consolidation this was folded into, if any
provenance   the source span it came from
```

The `kind` matters because it drives decay. A **case** is an episode: what you
did last fortnight matters, what you did last spring usually does not, so its
relevance halves every two weeks by default. A **strategy** is a learned
approach, and outlives the day you worked it out, so its half-life is a year.
**Hybrid** sits between at two months. See
[Retrieval](retrieval.md#decay) for what relevance does.

`occurred_at` and `ingested_at` being separate columns is what makes temporal
questions work. A session in August that discusses a meeting in June produces
an experience that occurred in June and was ingested in August, and "what
happened in June" finds it.

## Skills

A registry of callable things: name, kind, description, signature, body. Used
by dock-on agents at boot to discover what they can call, through
`anamnesia_capabilities`.

Skills are the one domain with **no vector channel**. They are served only by
lexical search, which is worth knowing before touching the lexical channel:
see [Retrieval](retrieval.md#the-lexical-channel).

## Working memory

Per-session scratch entries, tagged by role (`observation`, `plan`, `state`,
`tool_output`), each with a position in the session's trail and a TTL. A
worker purges expired ones every `worker.forget_every`.

Entries can be folded into a durable experience, which sets `folded_into` and
leaves the trail intact.

## Entities and edges

A bitemporal graph. Entities have a kind and a name; edges are typed and carry
their own validity window, so "who owned this in March" is a different answer
from "who owns it now" and both are stored.

Graph extraction is **off by default** (`graph.extract`), because it costs an
extra model call per checkpoint and an install that never reads the graph
should not pay for it. Turning it on affects new checkpoints only.

Entity identity is `(scope, kind, name)`, and deciding whether a newly
extracted entity is one you already have is a model's judgement, not a
distance threshold. `graph.candidate_distance` only controls which existing
entities get offered to the model as possible matches.

## Artifacts

Every page Claude Code publishes to claude.ai is recorded as it happens: the
URL, the title, the description, the project, and the readable text of the
page at publish time. Subagents are included, because tool hooks fire inside
them.

**Artifacts are the one domain that does not go through extraction**, and the
reason is worth stating. The URL is in the transcript either way, so nothing
is at risk of being lost. What the extractor would add is a judgement about
whether the page was interesting, applied to an identifier that is either
exactly right or useless. So the `PostToolUse` hook parses the tool's own
response, reads the published file while it still exists, and writes the row.
No source, no surprise gate, no model.

Identity is `(user, artifact_uuid)`. Republishing a file redeploys to the same
URL, so it updates the row instead of adding one.

`Body` is empty for an artifact recovered from a transcript after its source
file was cleaned up, which is the usual case for anything older than the
current session. The pointer is still worth having, and a later republish
fills the body in.

```bash
anamnesia artifacts            # list them
anamnesia artifacts backfill   # recover ones published before the hook existed
```

`backfill` is idempotent, so it doubles as the repair path for anything the
hook missed because the server was down.

`retrieval.artifact_max_distance` (default 0.60) is how close a match has to
be before a link is put in front of you unasked. Set it to 0 to keep the
listing and stop the prompt-driven surface.

## What happens to the raw text

A checkpoint lands in `sources` with its raw content. The extractor reads it.
`sources.raw_content` expires after **seven days**.

The row itself stays, because provenance points at it. What goes is the
transcript text, which has done its job by then.

## Consolidation

A daily worker clusters similar experiences and distills each cluster into one
higher-abstraction record, so memory gets shorter as it gets older rather than
growing without bound. The originals keep a `parent_id` pointing at the
distillation.

Two settings govern it, and both have measured defaults:

- `worker.consolidate_similarity` (0.65) is how alike two experiences must be
  before they fold. This shipped hardcoded at 0.85, which no real corpus
  reaches: over 1,402 same-scope pairs on a live install the mean was 0.289
  and the single most similar pair scored 0.754. Nothing ever clustered, and
  every pass reported success while folding nothing.
- `worker.consolidate_max_cluster` (8) caps how many experiences one insight
  is distilled from. A full cluster does not spill; the next similar
  experience opens a second one, so a long thread becomes several coherent
  summaries rather than one incoherent one.

## Related

- [Extraction](extraction.md), how a source becomes these rows
- [Retrieval](retrieval.md), how they come back
- [HTTP API](reference/http-api.md), browsing them directly
