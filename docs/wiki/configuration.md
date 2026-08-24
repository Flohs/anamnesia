# Configuring

Every setting is declared once, in `cmd/anamnesia/settings.go`. That one table
drives the generated config file and its comments, `anamnesia config`,
validation, tab completion, and the environment handed to the server process.

This page is how configuration behaves. For the settings themselves, see the
[config reference](reference/config.md).

## Where values come from

Highest priority wins:

1. **Command-line flags**
2. **Environment variables**, `ANAMNESIA_*` plus provider keys like
   `OPENROUTER_API_KEY`
3. **The project file**, `./.anamnesia.toml` in the current repository
4. **The global file**, `~/.anamnesia/config.toml`
5. **The built-in default**

`anamnesia config` prints the resolved value of every setting and which of
those five it came from, which is the fast way to answer "why is it using
that".

## The commands

```bash
anamnesia config                     # every setting, its value, and its source
anamnesia config path                # where the global file is
anamnesia config edit                # open it in $EDITOR
anamnesia config get llm.provider    # one resolved value
anamnesia config set embed.dims 3072 # write one value
```

Secrets are masked in output unless you pass `--show-secrets`.

To write to the project file instead of the global one:

```bash
anamnesia config --project set identity.project my-service
```

## Editing the file directly

`~/.anamnesia/config.toml` is generated with a comment above every setting,
taken from the same table the CLI reads. Editing it by hand is expected, and
writes made through `anamnesia config set` preserve your comments and
formatting rather than regenerating the file.

Restart the server after editing:

```bash
anamnesia restart
```

## Invalid values are rejected, not ignored

A bad value is an error naming the setting, at the moment you set it:

```
$ anamnesia config set embed.dims banana
Error: embed.dims must be a number, got "banana"

$ anamnesia config set rerank.provider gemini
Error: rerank.provider must be one of none, cohere, openrouter, got "gemini"
```

This is deliberate and worth understanding, because the alternative is worse.
A silent fallback to a default means the typo you made on Monday shows up as
inexplicable behaviour on Thursday, with nothing anywhere connecting the two.
Anamnesia never defaults silently.

Numbers are validated for sign as well as syntax. Most numeric settings are
sizes or intervals that cannot be zero, so `0` is rejected unless the setting
specifically treats zero as "off". The ones that do are marked in the
[reference](reference/config.md): `ingest.flush_bytes`, `ingest.flush_after`,
`ingest.segment_gap`, `ingest.segment_max_bytes` and
`retrieval.artifact_max_distance`.

Fractions are bounded to 0 through 1 by their kind rather than per setting,
which is the same bound the server enforces when it reads the value back. Both
ends of the path agreeing is the point.

## Global, project, and what belongs where

**Global** (`~/.anamnesia/config.toml`) holds everything, and is the only file
that may hold secrets. It is yours, not the repository's.

**Project** (`./.anamnesia.toml`) holds the handful of settings worth pinning
per repository. `anamnesia init` writes it. It is meant to be committed, which
is why a secret setting can never be one of them: the type system in
`settings.go` prevents it.

The settings that can appear in a project file are marked in the
[reference](reference/config.md). In practice it is `identity.project`,
`identity.user` and `server.url`.

## Using your own Postgres

Set `postgres.url` and Anamnesia manages no container at all. Every other
`postgres.*` setting is ignored. The database needs the `pgvector` extension
available.

```bash
anamnesia config set postgres.url 'postgres://user:pw@host:5432/anamnesia'
anamnesia restart
```

## Exposing the server

`server.addr` defaults to `127.0.0.1:8181`. Binding anything else exposes your
memory to the network, so set `server.token` as well:

```bash
anamnesia config set server.addr 0.0.0.0:8181
anamnesia config set server.token "$(openssl rand -hex 32)"
```

Clients then send `Authorization: Bearer <token>`. `/v1/health` is the one
route that stays unauthenticated, so a load balancer can probe it.

See [SECURITY.md](../../SECURITY.md) before doing this.

## Settings that need more than a restart

Two settings change the shape of the database, so setting them is only half
the job:

**`embed.dims`** must match the width of your embedding model, and the
database columns are already the old width. Rebuild them:

```bash
anamnesia config set embed.dims 3072
anamnesia migrate --dims 3072
anamnesia restart
```

That discards stored vectors and re-embeds them in the background. Facts and
experiences are not lost. The server refuses to boot on a mismatch rather than
failing one embedding write at a time.

**`graph.extract`** costs one extra model call per checkpoint and is off by
default. Turning it on affects only new checkpoints; it does not backfill a
graph over what you already have.

## The settings most people touch

| Setting | Default | What it does |
|---|---|---|
| `openrouter.api_key` | unset | One key fronts chat, embeddings and rerank |
| `llm.provider` | auto | `anthropic`, `openai`, `openrouter` or `stub` |
| `embed.provider` | auto | `openai`, `openrouter` or `stub` |
| `embed.dims` | `1536` | Must match your embedding model |
| `rerank.provider` | auto | `cohere` or `openrouter`. Latency for precision |
| `identity.user` | your username | Who memories belong to |
| `identity.project` | directory name | What they are filed under |
| `server.addr` | `127.0.0.1:8181` | Loopback by default |
| `postgres.port` | `5434` | Host port for the managed container |
| `graph.extract` | `false` | Entity and relationship extraction |
| `worker.extract_commitments` | `false` | Record open obligations |

The remaining 44 are in the [reference](reference/config.md).

## Tab completion

Completion knows the settings table, so it completes keys and the values that
go with them:

```
$ anamnesia config set embed.<TAB>
embed.provider  -- Vector embeddings for retrieval
embed.model     -- Leave empty for the provider default
embed.dims      -- Embedding width

$ anamnesia config set rerank.provider <TAB>
none  cohere  openrouter
```

The installed script only calls `anamnesia __complete`, so it never goes
stale. New settings come from whichever binary is on your `PATH`, and
upgrading does not need the script rewritten.
