# Anamnesia

Local-first long-term memory for AI agents. One Go binary, one Postgres,
one MCP endpoint. Anamnesia ingests anything you give it — Claude Code
chat turns, meeting transcripts, notes — extracts the durable bits with
an LLM, and serves them back via hybrid retrieval and an MCP tool surface.

## Getting started

You'll need:

1. **Docker** (for Postgres + the server container).
2. **Claude Code** (the client; hooks + MCP wiring is what Anamnesia patches).
3. **An Anthropic API key** if you want real extraction. Without one the
   pipeline runs end-to-end but every ingest is a no-op (stub LLM).
4. **Optionally an OpenAI key** for real embeddings instead of the
   deterministic stub. Recommended.

Five steps:

```bash
# 1. clone + build the binary
git clone https://github.com/flohs/anamnesia.git
cd anamnesia
make build                              # ./bin/anamnesia

# 2. configure
cp .env.example .env
# Edit .env. For real memory you need an LLM provider + an embed provider.
# Pick ONE LLM:
#   ANAMNESIA_LLM_PROVIDER=anthropic     ANTHROPIC_API_KEY=sk-ant-…
# or
#   ANAMNESIA_LLM_PROVIDER=openai        OPENAI_API_KEY=sk-…
# Embeddings (reuses OPENAI_API_KEY; OPENAI_BASE_URL works for OpenRouter/vLLM/Ollama):
#   ANAMNESIA_EMBED_PROVIDER=openai
# Leave both as `stub` if you just want to kick the tyres.

# 3. start the local stack (postgres + server)
./bin/anamnesia up                      # docker compose up -d --build
./bin/anamnesia doctor                  # confirm /v1/health is green

# 4. wire Claude Code
sudo cp bin/anamnesia /usr/local/bin/   # so the hooks can find it on PATH
./bin/anamnesia install                 # patches ~/.claude/settings.json + ~/.claude.json
./bin/anamnesia init                    # writes .anamnesia.toml in this project

# 5. open Claude Code. Anamnesia is in your context now.
```

That's the whole setup. From here every chat turn flows through the
extractor; durable preferences and decisions you mention in passing
become facts the next session loads automatically.

## What you can configure

Everything is environment variables. Full list in `.env.example`; the
ones you'll actually touch:

| Variable | Default | Why |
|---|---|---|
| `ANAMNESIA_LLM_PROVIDER` | `stub` | `anthropic`, `openai`, or `stub`. The extractor + consolidation workers use this. |
| `ANTHROPIC_API_KEY` | _unset_ | Required if `anthropic`. Default model: `claude-sonnet-4-6`. |
| `OPENAI_API_KEY` | _unset_ | Required if `openai` (LLM and/or embeddings). Default LLM model: `gpt-4o-mini`. Works against OpenAI, OpenRouter, vLLM, Ollama, Azure via `OPENAI_BASE_URL`. |
| `ANAMNESIA_EMBED_PROVIDER` | `stub` | Set to `openai` for real semantic retrieval. Reuses `OPENAI_API_KEY` / `OPENAI_BASE_URL`. |
| `ANAMNESIA_RERANK_PROVIDER` | `none` | Set to `cohere` + `COHERE_API_KEY` for higher-quality top-K. |
| `ANAMNESIA_PII_PROVIDER` | `regex` | In-process scrub. Set to `presidio` for the production sidecar. |
| `ANAMNESIA_PII_MODE` | `tag` | `redact` rewrites matches as `[EMAIL]` etc. before persisting. |
| `ANAMNESIA_SERVER_TOKEN` | _unset_ | Optional shared secret. Required when serving outside loopback. |
| `ANAMNESIA_HTTP_PORT` | `8181` | Host-side port; bump if it collides. |

For a small team: each member uses a distinct `ANAMNESIA_USER` (or
`--user` flag); memory partitions by user automatically.

## How it works (one paragraph)

Anything you POST to `/v1/ingest` (or that the `UserPromptSubmit` hook
sends through it) lands in a `sources` row. A background worker reads
pending sources, runs a cheap surprise gate, fetches the top-K similar
existing memories, and asks the LLM to emit `ADD_FACT / UPDATE_FACT /
DELETE_FACT / ADD_EXPERIENCE / NOOP` operations. The default is NOOP —
most chat content is noise. What survives lands in five typed domains:
**facts** (keyed claims), **experiences** (time-stamped narratives),
**skills** (callable registry), **working memory** (in-session TTL),
**entities + edges** (bitemporal graph). Retrieval fuses pgvector ANN
with tsvector lexical search (RRF), optionally Cohere-reranked. A
decay worker recomputes per-experience relevance hourly; a daily
consolidation worker clusters similar experiences and distils each
cluster into one higher-abstraction record. Raw `sources.raw_content`
TTLs out in 7 days unless you set `preserve_raw=true`.

## Subcommands

```
anamnesia up         start docker compose
anamnesia down       stop docker compose
anamnesia install    patch Claude Code's settings.json + .claude.json
anamnesia uninstall  remove the patches
anamnesia init       write .anamnesia.toml in the current project
anamnesia hook ...   internal — invoked by Claude Code's hooks
anamnesia doctor     diagnose config + connectivity
anamnesia serve      run the server (inside docker)
anamnesia migrate    apply database migrations and exit
```

## License

MIT — see [LICENSE](LICENSE).
