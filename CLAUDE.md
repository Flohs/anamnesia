# Anamnesia

Local-first long-term memory for Claude Code. One Go binary, one Postgres
container, one config file.

The binary is the CLI, the Claude Code hooks, and the server. All three are
the same executable running on the host, so they cannot end up on different
versions. The only containerised part is Postgres, which the binary manages
itself through the `docker` CLI: there is no compose file and no Anamnesia
image.

## Layout

- `cmd/anamnesia`: CLI, hooks, server entrypoint.
  - `settings.go`: **every** setting, declared once. Drives the generated
    config file, `anamnesia config`, validation, and the server's
    environment. Add settings here and nowhere else.
  - `hostconfig.go`: resolution (flags > env > project file > global file >
    defaults) and comment-preserving writes.
  - `docker.go` / `stack.go`: the Postgres container and the server process.
  - `install.go`: patches Claude Code's `settings.json` and MCP config.
  - `hook.go`: the four hooks.
  - `doctor.go`: install verification; exits non-zero on failure.
- `internal/`: store, retrieval, extraction, workers, HTTP, MCP.
- `~/.anamnesia/`: user state: `config.toml`, `server.log`, `server.pid`,
  `hooks.log`, `offsets/`. Override the root with `ANAMNESIA_HOME` (this is
  what the tests use).

## What it does

Six hooks: `SessionStart` loads memory into the session,
`UserPromptSubmit` retrieves for the prompt, `PreCompact` and
`SessionEnd` checkpoint the conversation, `Stop` flushes mid-session once
enough has accumulated, and `SubagentStop` records what a subagent
concluded. Checkpoints are **incremental**: a per-session byte offset in
`~/.anamnesia/offsets/` means each one sends only what was added since
the last.

`Stop` was removed once and is back gated, which is worth understanding
before touching it. It fires after every assistant turn, and it was
retired because it re-sent the whole transcript each time, so ingest grew
with the square of the session length. The offset fixed that: each flush
now sends only what is new, so ten flushes across a session send the same
bytes as one at the end, cut the same way, plus one trailing partial
segment each. What still must not happen is flushing *every* turn —
`ingest.flush_bytes` and `ingest.flush_after` are what stop that, and
setting both to 0 returns to checkpointing only at the end.

A session that crashes fires neither `PreCompact` nor `SessionEnd`, so
its last stretch is never checkpointed. It is not lost: the transcript is
on disk and the offset says how far we read. `anamnesia recover` collects
those tails, and `SessionStart` spawns it detached.

A checkpoint, `POST /v1/ingest`, or the `anamnesia_ingest` MCP tool lands as
a `sources` row. A background extractor reads pending sources, runs a
surprise gate, fetches top-K similar memories, asks the LLM for `ADD_FACT /
UPDATE_FACT / DELETE_FACT / ADD_EXPERIENCE / NOOP` operations, and executes
them. Default is NOOP, because most chat is noise; we don't save the conversation,
we extract what matters. Raw `sources.raw_content` TTLs out in 7 days.

Memory model — five typed domains:
- `facts` — keyed claims (preferences, project config).
- `experiences` — time-stamped narratives with `abstraction` level,
  `occurred_at`, `participants`, `topic`, `parent_id`, `provenance`.
- `skills` — callable registry.
- `working_memory` — in-session entries, TTL'd.
- `entities` + `edges` — bitemporal graph for multi-hop reasoning.

Retrieval: pgvector ANN + tsvector lexical, RRF-fused, optional Cohere rerank,
decay-aware scoring on experiences (`relevance` recomputed hourly).

## Invariants worth not breaking

- **The schema width and `embed.dims` must agree.** A mismatch makes every
  embedding write fail. `serve` refuses to boot on a mismatch; `migrate
  --dims N` is the repair. This shipped broken once.
- **`install` owns any hook running `anamnesia hook`**, not only entries
  carrying the `_anamnesia_managed` marker. Keying off the marker alone
  appended a second copy of every hook for anyone upgrading.
- **Hooks are written with the absolute binary path.** The shell Claude Code
  spawns often lacks `/usr/local/bin` on its PATH.
- **Hooks never break a session**: they exit 0 whatever happens, and record
  the outcome in `hooks.log` so `doctor` can report a hook that silently
  fails every turn.
- **Never default silently.** A bad config value is an error naming the
  setting, not a quiet fallback.
- **`/v1/health` must be able to fail.** It checks the database, schema
  version, embedding width and ANN indexes. A health check that cannot fail
  turns a broken install into a green light.
- **Retrieval must be able to fail, for the same reason.** A configured
  embedder that errors returns an error from `Search`, not an empty
  result set. A credit outage once had `/v1/retrieve` answer `200` with
  no hits for a user holding hundreds of fully-embedded facts, which is
  indistinguishable from "you have no such memory". Having *no* embedder
  stays legitimate: that is the lexical-only local setup.
- **The lexical channel earns nothing on extracted memory, and that is
  measured, not assumed.** Do not "fix" or expand it without new
  evidence, and do not delete it either. Measured 2026-08-21 over a
  30-question LongMemEval corpus (see
  [docs/longmemeval-retrieval-baseline.md](docs/longmemeval-retrieval-baseline.md)):
  `plainto_tsquery` ANDs every term, so a natural-language question
  matched **0 of 600** hits. Making it OR its terms and indexing the
  words inside dotted keys lifted it to 236 of 600 hits and changed
  recall by nothing at all, while costing `recall@5` (0.688 → 0.671) and
  crowding the graph channel out of the fused top-20 (`graph_only`
  37 → 16). No `LexicalK` from 5 to 40 made it a net win. Exact rare-token
  recall was **0.947 with it and 0.947 without**, so it does not earn its
  keep on identifier lookup either.
  It stays because deleting it is not free: `DomainSkill` has **no vector
  channel at all** and is served only by `lexicalSkills`, and every test
  in `internal/retrieval` runs without an embedder, so lexical is the
  only channel those tests can produce hits with. Removing it would take
  skills retrieval with it and require rewriting the graph tests.

## Verifying a change

```bash
make lint                                   # gofmt, vet, tests (starts a test DB)
export ANAMNESIA_HOME=/tmp/anamnesia-dev    # never touch the real install
./bin/anamnesia setup --no-hooks --no-start
./bin/anamnesia start && ./bin/anamnesia doctor --deep
```

Go may not be installed on the host; run the toolchain in a container if so.

## Behavioral guidelines

Behavioral guidelines to reduce common LLM coding mistakes. Merge with project-specific instructions as needed.

**Tradeoff:** These guidelines bias toward caution over speed. For trivial tasks, use judgment.

## 1. Think Before Coding

**Don't assume. Don't hide confusion. Surface tradeoffs.**

Before implementing:
- State your assumptions explicitly. If uncertain, ask.
- If multiple interpretations exist, present them - don't pick silently.
- If a simpler approach exists, say so. Push back when warranted.
- If something is unclear, stop. Name what's confusing. Ask.

## 2. Simplicity First

**Minimum code that solves the problem. Nothing speculative.**

- No features beyond what was asked.
- No abstractions for single-use code.
- No "flexibility" or "configurability" that wasn't requested.
- No error handling for impossible scenarios.
- If you write 200 lines and it could be 50, rewrite it.

Ask yourself: "Would a senior engineer say this is overcomplicated?" If yes, simplify.

## 3. Surgical Changes

**Touch only what you must. Clean up only your own mess.**

When editing existing code:
- Don't "improve" adjacent code, comments, or formatting.
- Don't refactor things that aren't broken.
- Match existing style, even if you'd do it differently.
- If you notice unrelated dead code, mention it - don't delete it.

When your changes create orphans:
- Remove imports/variables/functions that YOUR changes made unused.
- Don't remove pre-existing dead code unless asked.

The test: Every changed line should trace directly to the user's request.

## 4. Goal-Driven Execution

**Define success criteria. Loop until verified.**

Transform tasks into verifiable goals:
- "Add validation" → "Write tests for invalid inputs, then make them pass"
- "Fix the bug" → "Write a test that reproduces it, then make it pass"
- "Refactor X" → "Ensure tests pass before and after"

For multi-step tasks, state a brief plan:
```
1. [Step] → verify: [check]
2. [Step] → verify: [check]
3. [Step] → verify: [check]
```

Strong success criteria let you loop independently. Weak criteria ("make it work") require constant clarification.

---

**These guidelines are working if:** fewer unnecessary changes in diffs, fewer rewrites due to overcomplication, and clarifying questions come before implementation rather than after mistakes.


