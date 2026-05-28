# Anamnesia

Local-first long-term memory for AI agents. One Go binary, one Postgres,
one MCP endpoint. Anamnesia ingests anything you give it — Claude Code
chat turns, meeting transcripts, notes — extracts the durable bits with
an LLM, and serves them back via hybrid retrieval and an MCP tool surface.

## Getting started

You'll need:

1. **Docker** (for Postgres + the server container).
2. **Claude Code** (the client; hooks + MCP wiring is what Anamnesia patches).
3. **One API key** for the LLM + embeddings + rerank. Easiest path is
   **OpenRouter** — a single `OPENROUTER_API_KEY` fronts all three and
   lets you pick models from any provider on their platform. Direct
   Anthropic / OpenAI / Cohere keys still work if you prefer.

Five steps:

```bash
# 1. clone + build the binary
git clone https://github.com/flohs/anamnesia.git
cd anamnesia
make build                              # ./bin/anamnesia

# 2. configure
cp .env.example .env
# Edit .env. For real memory you need an LLM provider + an embed provider.
# Simplest: one key fronts everything.
#   OPENROUTER_API_KEY=sk-or-v1-…
# Auto-lights-up chat (anthropic/claude-sonnet-4.6), embeddings
# (openai/text-embedding-3-small), and rerank (cohere/rerank-v3.5).
# Override any model with ANAMNESIA_{LLM,EMBED,RERANK}_MODEL.
#
# Or use direct provider keys instead:
#   ANAMNESIA_LLM_PROVIDER=anthropic     ANTHROPIC_API_KEY=sk-ant-…
#   ANAMNESIA_EMBED_PROVIDER=openai      OPENAI_API_KEY=sk-…
#   ANAMNESIA_RERANK_PROVIDER=cohere     COHERE_API_KEY=…
#
# Leave everything unset if you just want to kick the tyres (stub LLM,
# stub embedder, no rerank).

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
| `OPENROUTER_API_KEY` | _unset_ | One key fronts chat + embeddings + rerank via [openrouter.ai](https://openrouter.ai). When set, all three providers auto-default to `openrouter` (override individually with the matching `ANAMNESIA_*_PROVIDER` var). |
| `ANAMNESIA_LLM_PROVIDER` | `stub` (or `openrouter` if `OPENROUTER_API_KEY` is set) | `anthropic`, `openai`, `openrouter`, or `stub`. The extractor + consolidation workers use this. |
| `ANTHROPIC_API_KEY` | _unset_ | Required if `anthropic`. Default model: `claude-sonnet-4-6`. |
| `OPENAI_API_KEY` | _unset_ | Required if `openai` (LLM and/or embeddings). Default LLM model: `gpt-4o-mini`. Works against OpenAI, vLLM, Ollama, Azure via `OPENAI_BASE_URL`. |
| `ANAMNESIA_EMBED_PROVIDER` | `stub` (or `openrouter` if `OPENROUTER_API_KEY` is set) | `openai`, `openrouter`, or `stub`. |
| `ANAMNESIA_RERANK_PROVIDER` | `none` (or `openrouter` if `OPENROUTER_API_KEY` is set) | `cohere` + `COHERE_API_KEY`, or `openrouter`, for higher-quality top-K. |
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
