# Configuration reference

All 55 settings. Mirrors `cmd/anamnesia/settings.go`, which is the only place
settings are declared. If this page and that file disagree, the file is right.

For how values resolve and where to put them, see
[Configuring](../configuration.md).

**Legend.** *Env* is the environment variable that overrides the file.
*Project* marks a setting that may appear in a repository's `.anamnesia.toml`.
*Zeroable* marks a numeric setting where `0` means "off" rather than "unset";
every other numeric setting rejects zero.

---

## identity

### `identity.user`
String. Default: your OS username. Env: `ANAMNESIA_DEFAULT_USER`. Project.

Who memories belong to. A small team can share one server by giving each
person a distinct handle.

### `identity.project`
String. Default: the git repository's directory name. Project.

Project slug memories are filed under. Belongs in a repository's
`./.anamnesia.toml`, which `anamnesia init` writes.

---

## Provider keys

### `openrouter.api_key`
Secret. Env: `OPENROUTER_API_KEY`.

One key fronts chat, embeddings and rerank. Setting it switches all three
providers to `openrouter` unless you pick them explicitly. Get one at
<https://openrouter.ai/keys>.

### `openai.api_key`
Secret. Env: `OPENAI_API_KEY`.

Needed for `llm.provider=openai` or `embed.provider=openai`.

### `openai.base_url`
String. Default: `https://api.openai.com/v1`. Env: `OPENAI_BASE_URL`.

Point this at vLLM, Ollama or Azure to use an OpenAI-compatible endpoint
instead.

### `anthropic.api_key`
Secret. Env: `ANTHROPIC_API_KEY`.

Needed for `llm.provider=anthropic`.

### `cohere.api_key`
Secret. Env: `COHERE_API_KEY`.

Needed for `rerank.provider=cohere`.

---

## llm

### `llm.provider`
Enum: `stub`, `anthropic`, `openai`, `openrouter`. Default: auto.
Env: `ANAMNESIA_LLM_PROVIDER`.

Extraction and consolidation model. Leave empty to auto-pick: `openrouter`
when `openrouter.api_key` is set, otherwise `stub`. The stub extracts nothing,
which is fine for trying things out.

### `llm.model`
String. Default: per provider. Env: `ANAMNESIA_LLM_MODEL`.

Leave empty for the provider default: `anthropic/claude-sonnet-4.6` on
openrouter, `claude-sonnet-4-6` direct, `gpt-4o-mini` on openai.

### `llm.timeout`
Duration. Default: `120s`. Env: `ANAMNESIA_LLM_HTTP_TIMEOUT`.

Per-request HTTP timeout. Raise it a lot for a cold local model.

---

## embed

### `embed.provider`
Enum: `stub`, `openai`, `openrouter`. Default: auto.
Env: `ANAMNESIA_EMBED_PROVIDER`.

Vector embeddings for retrieval. Same auto-pick rule as `llm.provider`.

### `embed.model`
String. Default: per provider. Env: `ANAMNESIA_EMBED_MODEL`.

Leave empty for the provider default (`text-embedding-3-small`).

### `embed.dims`
Int. Default: `1536`. Env: `ANAMNESIA_EMBED_DIMS`.

Embedding width. Must match your model: 1536 for `text-embedding-3-small`,
3072 for `-3-large`, 768 for `nomic-embed-text`.

Changing this needs `anamnesia migrate --dims N`, which rebuilds the columns
and discards existing vectors so they can be re-embedded. The server refuses
to boot on a mismatch.

---

## rerank

### `rerank.provider`
Enum: `none`, `cohere`, `openrouter`. Default: auto.
Env: `ANAMNESIA_RERANK_PROVIDER`.

Optional second-pass scoring of search candidates. Costs latency, buys
precision. Also what gives the surprise gate an absolute score to judge with;
see [Extraction](../extraction.md#step-1-the-surprise-gate).

### `rerank.model`
String. Default: per provider. Env: `ANAMNESIA_RERANK_MODEL`.

Leave empty for the provider default (`cohere/rerank-v3.5` on openrouter).

---

## server

### `server.addr`
String. Default: `127.0.0.1:8181`. Env: `ANAMNESIA_HTTP_ADDR`.

Address the local server listens on. Loopback by default; anything else
exposes your memory to the network, so set `server.token` too.

### `server.url`
String. Default: derived from `server.addr`. Project.

Where the CLI and hooks look for the server. Set it to point at a server on
another machine.

### `server.token`
Secret. Env: `ANAMNESIA_SERVER_TOKEN`.

Optional shared secret. Required in practice whenever `server.addr` is not
loopback.

### `server.autostart`
Bool. Default: `true`.

Let hooks start the stack on demand when it is not running, so a new session
heals itself instead of silently losing memory.

### `server.shutdown_wait`
Duration. Default: `30s`. Env: `ANAMNESIA_SHUTDOWN_WAIT`.

How long the server may take to finish in-flight work when asked to stop.
`stop` and `restart` wait this out before reporting a server that will not
exit.

---

## postgres

### `postgres.url`
String. Default: unset.

Use an existing Postgres instead of a managed container. Needs the `pgvector`
extension available. When set, every other `postgres.*` setting is ignored and
Anamnesia manages no container.

### `postgres.image`
String. Default: `pgvector/pgvector:pg16`.

Image for the managed container.

### `postgres.container`
String. Default: `anamnesia-postgres`.

Container name. Anamnesia only ever touches a container with this exact name.

### `postgres.volume`
String. Default: `anamnesia-pgdata`.

Docker volume holding the data. Your memory lives here; removing it deletes
everything.

### `postgres.port`
Int. Default: `5434`.

Host port for the container, bound to loopback only. Change it if something
already uses 5434.

### `postgres.user`
String. Default: `anamnesia`.

### `postgres.password`
Secret. Generated at setup.

Only reachable from this machine. Regenerating a config does not overwrite it.

### `postgres.database`
String. Default: `anamnesia`.

---

## pii

### `pii.provider`
Enum: `none`, `regex`, `presidio`. Default: `regex`.
Env: `ANAMNESIA_PII_PROVIDER`.

Detects personal data before anything is stored. `regex` is in-process;
`presidio` calls a sidecar.

### `pii.mode`
Enum: `tag`, `redact`. Default: `tag`. Env: `ANAMNESIA_PII_MODE`.

`tag` records which categories were found; `redact` also replaces the matches
before storing.

### `pii.presidio_url`
String. Default: unset. Env: `ANAMNESIA_PRESIDIO_URL`.

Required when `pii.provider=presidio`.

---

## worker

### `worker.extract_every`
Duration. Default: `15s`. Env: `ANAMNESIA_EXTRACT_EVERY`.

How often the extractor drains newly ingested sources.

### `worker.embed_backfill`
Duration. Default: `1m`. Env: `ANAMNESIA_EMBED_BACKFILL`.

How often rows missing a vector get embedded.

### `worker.forget_every`
Duration. Default: `1h`. Env: `ANAMNESIA_FORGET_EVERY`.

How often expired working memory is purged.

### `worker.decay_every`
Duration. Default: `1h`. Env: `ANAMNESIA_DECAY_EVERY`.

How often experience relevance is recomputed.

### `worker.consolidate_every`
Duration. Default: `24h`. Env: `ANAMNESIA_CONSOLIDATE_EVERY`.

How often similar experiences are clustered and distilled.

### `worker.consolidate_similarity`
Fraction. Default: `0.65`. Env: `ANAMNESIA_CONSOLIDATE_SIMILARITY`.

How alike two experiences must be, as a cosine from 0 to 1, before
consolidation folds them into one insight.

This shipped hardcoded at 0.85, which no real corpus reaches: measured over
the 1,402 same-scope pairs on a live install, the mean was 0.289 and the
single most similar pair scored 0.754, so nothing ever clustered and every
pass reported success while folding nothing. 0.65 was chosen by replaying the
clusterer over that corpus and reading what it merged: it forms clean topical
pairs.

Lower it to 0.60 to fold whole threads rather than pairs, at the risk of a
summary that blurs its sources and then competes with them in retrieval.

### `worker.consolidate_max_cluster`
Int. Default: `8`. Env: `ANAMNESIA_CONSOLIDATE_MAX_CLUSTER`.

Most experiences one insight may be distilled from. A cluster that fills up
does not spill: the next similar experience opens a second cluster, so a
long-running thread becomes several summaries rather than one incoherent one.

Raise it if your threads are longer than eight sessions and you would rather
have one summary than several.

### `worker.extract_concurrency`
Int. Default: `1`. Env: `ANAMNESIA_EXTRACT_CONCURRENCY`.

How many sources the extractor works on at once. Extraction is mostly waiting
on the model, so raising this is close to a linear speedup and does not cost
more tokens.

It does change what is extracted: sources handled together stop seeing each
other's facts as merge candidates, so a bulk backfill of related sessions can
produce duplicates a serial drain would have merged. Raise it for benchmarks
and backfills; leave it at 1 for a live install where dedup matters more than
throughput.

### `worker.extract_commitments`
Bool. Default: `false`. Env: `ANAMNESIA_EXTRACT_COMMITMENTS`.

Also record open obligations ("I'll send X by Friday") in the commitments
ledger.

---

## retrieval

### `retrieval.artifact_max_distance`
Fraction. Default: `0.60`. Env: `ANAMNESIA_ARTIFACT_MAX_DISTANCE`. Zeroable.

How close a published artifact must be to a prompt, as a cosine distance from
0 (identical) to 1, before it is offered alongside the answer.

An artifact is a link put in front of someone who did not ask for one, so it
has to clear an absolute bar rather than merely rank first: a fused RRF score
cannot express that, because it gives the best of an irrelevant pool exactly
what it gives the best of a relevant one.

0.60 was measured over 33 real artifacts with `openai/text-embedding-3-small`:
matches a person would call relevant scored 0.32 to 0.63, and the closest
match for a prompt about something else scored 0.759, so the band between them
is wide and 0.60 sits inside it with no false positives.

Raise it toward 0.65 to catch the tail of that relevant range at the cost of
the occasional link that is not about the question; lower it to surface only
on a close match. Set it to 0 to stop surfacing artifacts on prompts entirely,
leaving `anamnesia artifacts` and the session-start list.

Recalibrate if you change embedding models: the bands are a property of the
model, not of Anamnesia.

---

## ingest

### `ingest.flush_bytes`
Int. Default: `16384`. Zeroable.

Checkpoint mid-session once this many new bytes of transcript have
accumulated. `Stop` fires after every assistant turn; this decides when that
turn is worth a checkpoint.

Bytes rather than turns because it is what lines up with segments: reaching it
means there is a segment's worth of new material to cut, so a flush produces
whole segments instead of slivers.

Because checkpoints are incremental, flushing often costs about the same as
flushing once at the end, the same bytes cut the same way; it just stops the
work waiting for the session to finish. Set to 0 to use only the time gate.

### `ingest.flush_after`
Duration. Default: `20m`. Zeroable.

Checkpoint mid-session once this long has passed since the last one, however
little has accumulated. The backstop for a slow conversation that never
reaches `ingest.flush_bytes` quickly but should still not sit uncheckpointed
for hours.

Set to 0 to use only the byte gate; set both to 0 to checkpoint only at
`PreCompact` and `SessionEnd`, which is what earlier versions did.

### `ingest.recover_stranded`
Bool. Default: `true`.

Ingest transcript tails from sessions that ended without a checkpoint. A
checkpoint fires on `PreCompact` and `SessionEnd`, so a session that crashes
or is killed never sends its last stretch of work.

The transcript is still on disk and the offset file records how far it was
read, so `anamnesia recover` reads the rest; session start runs it in the
background. Turn it off to leave abandoned tails alone.

### `ingest.recover_idle`
Duration. Default: `15m`.

How long a transcript must go unwritten before recovery treats its session as
over.

This is the only judgement recovery makes, and it cuts both ways: too short
and it ingests a live session's tail, racing that session's own checkpoint and
paying to extract content that is about to be sent again; too long and a
crashed session's work sits uncollected for longer. Nothing is lost either
way, since the transcript stays on disk until recovery reads it.

### `ingest.segment_gap`
Duration. Default: `20m`. Zeroable.

A pause longer than this starts a new segment when a checkpoint is cut up, so
the surprise gate judges one subject at a time rather than a whole session.
Set to 0 to send each checkpoint as a single source, which is what earlier
versions did.

### `ingest.segment_max_bytes`
Int. Default: `4000`. Zeroable.

A segment is cut when it grows past this, because a long unbroken session is
still not one idea. It is also what bounds how much the extractor has to hold
at once: attention degrades over a long input, so a bigger segment does not
mean more extracted, it means less.

Measured on three real 21KB sessions, the same content yielded 14 unique facts
at 32768 and 74 at 4000, and the ones only the smaller cap found were standing
preferences like "branch instead of committing directly to main", exactly what
memory is for. The cost is one model call per segment, so 3 calls became 18.

Raise it to spend less, set to 0 to disable the size cut.

---

## decay

### `decay.half_life_case`
Duration. Default: `336h` (2 weeks). Env: `ANAMNESIA_DECAY_HALF_LIFE_CASE`.

How long a remembered episode takes to lose half its relevance. What you did
last fortnight matters, what you did last spring usually does not. Recomputed
every `worker.decay_every`.

### `decay.half_life_strategy`
Duration. Default: `8760h` (1 year). Env: `ANAMNESIA_DECAY_HALF_LIFE_STRATEGY`.

The same for a learned approach rather than an episode. A year is close enough
to never: how you solve a problem outlives the day you solved it.

### `decay.half_life_hybrid`
Duration. Default: `1440h` (2 months). Env: `ANAMNESIA_DECAY_HALF_LIFE_HYBRID`.

The same for an experience that is part episode, part approach. Between the
other two.

---

## activity

### `activity.enabled`
Bool. Default: `true`. Env: `ANAMNESIA_ACTIVITY_ENABLED`.

Record what the server is doing, in memory, and serve it on `/v1/activity`.
Off makes those routes 404 and every recording call a no-op.

### `activity.traces`
Int. Default: `200`. Env: `ANAMNESIA_ACTIVITY_TRACES`.

How many recent traces to keep. They live in memory only, so a restart clears
them. Use `activity.enabled` to switch recording off; this is a size, and
sizes are positive.

---

## graph

### `graph.extract`
Bool. Default: `false`. Env: `ANAMNESIA_GRAPH_EXTRACT`.

Extract entities and relationships from a session, in one extra model call per
checkpoint. Off by default: it costs a call, and an install that never reads
the graph should not pay for it.

### `graph.max_ops`
Int. Default: `12`. Env: `ANAMNESIA_GRAPH_MAX_OPS`.

Caps how many entities and edges one checkpoint may produce.

### `graph.candidate_distance`
Fraction. Default: `0.45`. Env: `ANAMNESIA_GRAPH_CANDIDATE_DISTANCE`.

How close an existing entity's name must embed to a newly extracted entity's
name, and share its kind, before it is offered to the model as a possible
match, triggering one extra, otherwise-skipped model call per checkpoint to
ask whether the two are really the same thing.

Cosine distance from 0 to 1, so smaller is stricter. This does not merge
anything by itself: the model decides identity, so it is deliberately loose.
Raise it only if relevant entities are consistently missing from what the
model is offered. Past 1 the two names point away from each other, which is
not a candidate for being the same thing, so 1 is the ceiling.
