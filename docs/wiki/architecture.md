# Architecture

For working on the code. For running it, see
[Getting started](getting-started.md).

## One binary, three roles

`anamnesia` is the CLI, the Claude Code hooks, and the memory server. All
three are the same executable running on the host, so they cannot end up on
different versions. This is not a convenience: a hook written by version A
talking to a server running version B is a class of bug that simply does not
exist here.

The only containerised part is Postgres, which the binary manages itself
through the `docker` CLI. There is no compose file and no Anamnesia image.

## Layout

```
cmd/anamnesia/          CLI, hooks, server entrypoint
  settings.go           every setting, declared once
  hostconfig.go         resolution and comment-preserving writes
  docker.go / stack.go  the Postgres container and the server process
  install.go            patches Claude Code's settings.json and MCP config
  hook.go               the hooks
  hook_artifact.go      the PostToolUse path
  doctor.go             install verification; exits non-zero on failure
internal/
  store/                Postgres, migrations, every query
  retrieval/            fusion, channels, rerank
  extract/              the surprise gate, operations, graph extraction
  jobs/                 background workers
  httpapi/              the HTTP surface
  mcp/                  the MCP tool surface
  embed/ llm/ pii/      provider adapters
  activity/             traces
pkg/anamnesia/          the public types
```

`~/.anamnesia/` holds user state: `config.toml`, `server.log`, `server.pid`,
`hooks.log`, `offsets/`, `completions/`. Override the root with
`ANAMNESIA_HOME`, which is what the tests use.

## settings.go is the only place settings exist

One table drives the generated config file and its comments, `anamnesia
config`, validation, tab completion, and the environment handed to the server.

Add settings there and nowhere else. This is not style. Before it existed,
`docker-compose.yml`, `.env.example` and `internal/config` disagreed with each
other about what settings existed and what the defaults were.

## Invariants worth not breaking

These are load-bearing. Each one is here because breaking it shipped.

**The schema width and `embed.dims` must agree.** A mismatch makes every
embedding write fail. `serve` refuses to boot on a mismatch; `migrate --dims N`
is the repair. This shipped broken once.

**`install` owns any hook running `anamnesia hook`**, not only entries carrying
the `_anamnesia_managed` marker. Keying off the marker alone appended a second
copy of every hook for anyone upgrading.

**Hooks are written with the absolute binary path.** The shell Claude Code
spawns often lacks `/usr/local/bin` on its `PATH`.

**Hooks never break a session.** They exit 0 whatever happens, and record the
outcome in `hooks.log` so `doctor` can report a hook that silently fails every
turn.

**A new table with an embedding column must join `embeddingTables`.**
`migrate --dims` re-dimensions only the tables on that list, and
`EmbeddingDims` used to read `facts.embedding` alone as representative, so an
unregistered column kept the old width, rejected every embedding write, and
reported green throughout. `EmbeddingDims` now reads all of them and refuses
to answer when they disagree, and `TestEmbeddingTablesListsEveryEmbeddingColumn`
holds the list against the live schema so the omission fails on the migration
that causes it.

The same shape applies to `projectScopedTables`: a project-scoped table left
off it is silently stranded by `project move` and deleted as empty by
`project prune`. Both lists have tests that introspect the schema rather than
trusting the list.

**`Migrate` is serialised by a Postgres advisory lock, and has to be.**
Migrations are DDL and goose serialises nothing, so two processes migrating
the same *empty* database collide with "already exists" from the middle of a
migration file. This is invisible locally, because a long-lived test database
is already migrated and goose does nothing; it is reliable on CI, which always
starts empty and runs `go test ./...` as concurrent per-package processes
against one database. `serve` migrating at boot while someone runs `anamnesia
migrate` is the same race in production. `TestConcurrentMigrateIsSafe`
reproduces it in a fraction of a second against a throwaway database.

**A gate that cannot fail is not a gate.** `release.yml` ran `go test` without
a database, so every DB-backed test skipped and two releases published green
while CI was red on the same commit. Both workflows now run against pgvector.
Before trusting any green check, ask what it would take for it to go red.

**Never default silently.** A bad config value is an error naming the setting,
not a quiet fallback.

**`/v1/health` must be able to fail.** It checks the database, schema version,
embedding width and ANN indexes. A health check that cannot fail turns a
broken install into a green light.

**Retrieval must be able to fail, for the same reason.** A configured embedder
that errors returns an error from `Search`, not an empty result set. A credit
outage once had `/v1/retrieve` answer `200` with no hits for a user holding
hundreds of fully-embedded facts, which is indistinguishable from "you have no
such memory". Having *no* embedder stays legitimate: that is the lexical-only
local setup.

**The lexical channel earns nothing on extracted memory, and that is measured,
not assumed.** Do not "fix" or expand it without new evidence, and do not
delete it either. See [Retrieval](retrieval.md#the-lexical-channel) for the
numbers and for what removing it would take with it.

## Data flow

```
Claude Code session
   │ hooks
   ▼
sources row  ──► extractor ──► facts / experiences / entities / edges
   │                              │
   │ raw_content TTL 7d           ▼
   ▼                          embedding backfill
 expires                          │
                                  ▼
                        retrieval (vector + lexical + graph, RRF, rerank)
                                  │
                                  ▼
                          next session's context
```

Artifacts bypass the extractor entirely and are written by the hook. See
[Memory model](memory-model.md#artifacts).

## Background workers

| Worker | Cadence setting | Default |
|---|---|---|
| Extractor | `worker.extract_every` | 15s |
| Embedding backfill | `worker.embed_backfill` | 1m |
| Forget expired working memory | `worker.forget_every` | 1h |
| Decay | `worker.decay_every` | 1h |
| Consolidation | `worker.consolidate_every` | 24h |

## Verifying a change

```bash
make lint                                   # gofmt, vet, tests (starts a test DB)
export ANAMNESIA_HOME=/tmp/anamnesia-dev    # never touch the real install
./bin/anamnesia setup --no-hooks --no-start
./bin/anamnesia start && ./bin/anamnesia doctor --deep
```

Setting `ANAMNESIA_HOME` is not optional advice. Without it you are testing
against your own memory.

Go may not be installed on the host; run the toolchain in a container if so.

## Migrations

Goose, in `internal/store/migrations/`. Eleven of them as of schema v11.

Adding one that creates an embedding column means adding the table to
`embeddingTables`. Adding one that creates a project-scoped table means adding
it to `projectScopedTables`. Both have tests that introspect the live schema,
so forgetting fails on the migration that causes it rather than months later.

## See also

- [CONTRIBUTING.md](../../CONTRIBUTING.md)
- [SECURITY.md](../../SECURITY.md)
