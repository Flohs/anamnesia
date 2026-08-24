# Troubleshooting

## Start here

```bash
anamnesia doctor
```

Every failure it reports names the command that fixes it. It exits non-zero
when any check fails, so it works in a script.

```bash
anamnesia doctor --deep    # also writes and reads back a memory
anamnesia doctor --json    # for scripts
```

`--deep` exercises the exact path your sessions use, including the embedder.
It is the check to run after changing providers, because a broken embedder
looks fine to every other check.

Then:

```bash
anamnesia status        # is the database up? is the server up?
anamnesia logs -n 100   # why did the server stop?
anamnesia logs -f       # watch it live
tail -f ~/.anamnesia/hooks.log
```

## The server refuses to start and mentions `vector(N)`

Your configured `embed.dims` and the database schema disagree.

The server refuses to boot rather than failing one embedding write at a time,
because a width mismatch makes **every** embedding write fail, and doing that
quietly is how an install looks green while storing nothing.

Either set the value back, or rebuild the columns:

```bash
anamnesia migrate --dims 3072
anamnesia restart
```

Rebuilding discards stored vectors and re-embeds them in the background. Facts
and experiences are not lost.

Widths that go with common models: 1536 for `text-embedding-3-small`, 3072 for
`-3-large`, 768 for `nomic-embed-text`.

## Nothing is being remembered

In order of likelihood:

**1. There is no model.** Check `llm.provider` in `anamnesia config`. With
`stub` nothing is extracted, by design.

```bash
anamnesia config set openrouter.api_key sk-or-v1-…
anamnesia restart
```

**2. There is no embedder.** Without one there are no candidates and no
surprise gate. Check `embed.provider`.

**3. The queue is backed up.**

```bash
curl -s localhost:8181/v1/queue/pending
```

**4. It is working, and the answer is genuinely `NOOP`.** That is the default,
and on a lot of conversation it is correct. Read the traces:

```bash
curl -s localhost:8181/v1/activity | head
```

Each extraction records why the gate decided what it did, in words.

**5. Hooks are not firing at all.** See the next section.

## Claude Code is not picking Anamnesia up

Restart Claude Code. It reads hooks and MCP servers at startup, so a fresh
`anamnesia setup` does nothing for an already-running session.

Then `anamnesia doctor`, which reports duplicated hooks, hooks pointing at a
binary that no longer exists, and an MCP URL that does not match your server.

If `hook runs` says no hook has ever run, the wiring is not reaching the
binary. The usual cause is that the binary moved:

```bash
anamnesia install     # rewrites the hooks with the current absolute path
```

Hooks are written with an absolute path on purpose, because the shell Claude
Code spawns often lacks `/usr/local/bin` on its `PATH`.

## Hooks run but always fail

They exit 0 whatever happens, by design, so a session never breaks. The
failure is in the log:

```bash
tail -50 ~/.anamnesia/hooks.log
```

`anamnesia doctor` summarises the same file, which is the point of it existing.

## A port is already in use

```bash
anamnesia config set postgres.port 5435    # or server.addr for the API port
anamnesia start
```

## The database rejects your password

This happens when a data volume outlives the config that created it, because
Postgres keeps the password from when the volume was first initialised.

Anamnesia detects this and fixes it automatically. If it cannot, it explains
your options rather than guessing.

## The container will not start

```bash
docker ps -a --filter name=anamnesia-postgres
docker logs anamnesia-postgres
```

Anamnesia only ever touches a container with the exact name in
`postgres.container`. If you have your own Postgres, set `postgres.url` and no
container is managed at all.

## Retrieval returns nothing

First check that it is not lying to you. A configured embedder that errors
returns an error, not an empty result, precisely so this case is
distinguishable:

```bash
curl -s localhost:8181/v1/health
```

Health checks the database, schema version, embedding width and ANN indexes,
and can fail. A health check that cannot fail turns a broken install into a
green light.

If health is fine and searches are empty, you may genuinely have no memory
yet. Check what is stored:

```bash
curl -s localhost:8181/v1/stats
anamnesia artifacts
```

## Artifacts are missing

Artifacts published before the `PostToolUse` hook existed are still in your
transcripts:

```bash
anamnesia artifacts backfill
```

Most recover as a pointer without the page text, because a published file
lives in a session scratchpad that gets cleaned up. It is idempotent, so it is
also the repair path if the server was down when something was published.

If artifacts exist but are never offered alongside answers, check
`retrieval.artifact_max_distance`. At 0 the prompt-driven surface is off by
design. If you changed embedding models, the distance bands changed with them
and need recalibrating.

## A session crashed and its work is missing

It is not lost. The transcript is on disk and the offset file records how far
it was read.

```bash
anamnesia recover
```

`SessionStart` spawns this detached, so it usually happens on its own. If it
is not picking a session up, `ingest.recover_idle` (15m) is how long a
transcript must go unwritten before recovery treats the session as over.

## Everything is broken and I want to start over

```bash
anamnesia setup           # idempotent: repairs whatever drifted, keeps settings
```

That is almost always enough. If you truly want the memory gone:

```bash
anamnesia uninstall --purge     # also deletes the docker volume
```

`--purge` removes `anamnesia-pgdata`, which deletes everything, on purpose.
Without it your memory survives uninstall.

## Reading a health check

```bash
curl -s localhost:8181/v1/health | jq
```

It verifies the database connection, the schema version, the embedding width
and the ANN indexes. Each can fail independently, and the response says which
one did.

`/v1/health` is the one route that stays unauthenticated, so a load balancer
can probe it.

## Getting more detail

```bash
anamnesia serve -v      # info
anamnesia serve -vv     # debug
```

Or run any hook by hand to see what it does with a real payload:

```bash
echo '{"session_id":"…","transcript_path":"…"}' | anamnesia hook session-start
```
