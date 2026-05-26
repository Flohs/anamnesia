# Anamnesia

Local-first long-term memory for AI agents. Single Go binary, single Postgres,
single MCP endpoint. The same binary runs the server (inside docker) and the
Claude Code hooks (on the host).

## What it does

Anything pushed to `POST /v1/ingest` (or the `anamnesia_ingest` MCP tool, or
the `UserPromptSubmit` hook in the background) lands as a `sources` row.
A background extractor reads pending sources, runs a surprise gate, fetches
top-K similar memories, asks the LLM for `ADD_FACT / UPDATE_FACT /
DELETE_FACT / ADD_EXPERIENCE / NOOP` operations, and executes them. Default
is NOOP — most chat is noise; we don't save the conversation, we extract
what matters. Raw `sources.raw_content` TTLs out in 7 days.

Memory model — five typed domains:
- `facts` — keyed claims (preferences, project config).
- `experiences` — time-stamped narratives with `abstraction` level,
  `occurred_at`, `participants`, `topic`, `parent_id`, `provenance`.
- `skills` — callable registry.
- `working_memory` — in-session entries, TTL'd.
- `entities` + `edges` — bitemporal graph for multi-hop reasoning.

Retrieval: pgvector ANN + tsvector lexical, RRF-fused, optional Cohere rerank,
decay-aware scoring on experiences (`relevance` recomputed hourly).

## Layout

```
cmd/anamnesia/        single-binary entry: serve | hook | install | init | doctor | up | down | migrate
internal/
  config/             env-driven config
  store/              Postgres + pgvector; migrations 0001..0003
  embed/              OpenAI-compatible + stub embedders
  llm/                Anthropic + OpenAI + stub LLM (Complete / Distill / Extract)
  retrieval/          hybrid search + optional Cohere reranker
  pii/                regex + Presidio (tag or redact)
  decay/              relevance recompute + compaction
  extract/            ingest → surprise gate → LLM → ADD/UPDATE/DELETE ops
  jobs/               worker loops (embed, forget, decay, consolidate, extract, purge-sources)
  httpapi/            /v1/health, /sessions/start, /retrieve, /capture, /sessions/end, /ingest
  mcp/                MCP tools at /mcp (facts, experiences, skills, working, graph, audit, ingest)
pkg/anamnesia/        public domain types
docker-compose.yml    pgvector/postgres + anamnesia container
Dockerfile            multi-stage build of the binary
Makefile              build, test, up, down, migrate
```

## Common tasks

```bash
make build                                              # ./bin/anamnesia
make test                                               # unit tests
ANAMNESIA_TEST_DATABASE_URL=postgres://… make test      # integration tests (extract_test hits real DB)
make up    / make down                                  # docker compose lifecycle
make logs                                               # tail the anamnesia container
```

## Conventions

- **One binary, one go.mod.** Don't split packages by deployment role; the
  server and the host CLI are the same binary picking different subcommands.
- **Schema changes live in `internal/store/migrations/`** as goose-format
  `.sql` files. Always add a new numbered file; never edit a shipped one.
- **No multitenancy.** Single owner; multi-user via the `users` table for
  small teams. Don't reintroduce `tenant_id`.
- **Auth is optional.** `ANAMNESIA_SERVER_TOKEN` enforces a shared secret
  for `/v1` + `/mcp`. Loopback-only stacks leave it empty.
- **PII before persist.** Every text path that writes to facts/experiences
  goes through `pii.Detector.Scrub` (configurable `tag` vs `redact`).
- **Hooks are best-effort.** Failures in `anamnesia hook …` are swallowed —
  Claude is never blocked on memory work.

## Configuration knobs

LLM (`ANAMNESIA_LLM_PROVIDER`): `stub` | `anthropic` | `openai`. The latter
two need `ANTHROPIC_API_KEY` / `OPENAI_API_KEY`. OpenAI provider works
against any OpenAI-compatible endpoint via `OPENAI_BASE_URL` (OpenRouter,
vLLM, Ollama, Azure). Default models pick themselves per provider.

Embeddings (`ANAMNESIA_EMBED_PROVIDER`): `stub` | `openai` (reuses
`OPENAI_API_KEY`/`OPENAI_BASE_URL`).

Rerank (`ANAMNESIA_RERANK_PROVIDER`): `none` | `cohere` (+ `COHERE_API_KEY`).

PII (`ANAMNESIA_PII_PROVIDER`): `none` | `regex` | `presidio`;
`ANAMNESIA_PII_MODE` is `tag` (default) or `redact`.

Worker cadences (Go duration strings) — `ANAMNESIA_{EMBED_BACKFILL,
FORGET_EVERY, DECAY_EVERY, CONSOLIDATE_EVERY, EXTRACT_EVERY}`.

Full list in `.env.example`.
