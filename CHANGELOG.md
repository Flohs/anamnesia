# Changelog

## Unreleased

Anamnesia became a single self-contained CLI, and the install/update paths
were rebuilt around being verifiable.

### Fixed

- **A fresh install could not store a single memory.** The migration chain
  ended at `vector(2048)` while the default embedding width was 1536, so
  every embedding write failed with `expected 2048 dimensions, not 1536`.
  Migration `0008` returns the columns to the shipped default.
- **Vector search had no index at all.** Migration `0006` dropped the three
  HNSW indexes and nothing recreated them, so every similarity query was a
  sequential scan. `0008` rebuilds them.
- **The server now refuses to start** when the schema width and `embed.dims`
  disagree, naming both numbers and the command that fixes it, instead of
  failing one background write at a time.
- **`install` duplicated hooks.** Ownership was decided by a marker key, so
  hooks written by an earlier version (which had no marker) were kept and a
  second copy appended. Anything running `anamnesia hook` is now replaced.
  A binary whose name carries an extension (`anamnesia.exe`) is recognised
  too.
- **`doctor` reported a healthy install over a broken one.** `/v1/health` was
  a static `{"ok":true}` that never touched Postgres. It now checks the
  database, the schema version, the embedding width, the ANN indexes and the
  running version, and `doctor` exits non-zero when anything fails.
- **`doctor` told users to run `install` when hooks were already present**,
  which is what duplicated them. It detects hooks by command, and reports
  duplicates, missing events, stale events and unusable binary paths.
- **Hooks were written as the bare name `anamnesia`**, which only works when
  the shell Claude Code spawns happens to have the binary on its PATH. They
  now carry the absolute, symlink-resolved path.
- **`Stop` was used as if it were session end.** It fires after every
  assistant turn, so each turn re-sent the entire growing transcript:
  quadratic ingest volume and repeated extraction of the same content at full
  model cost. Checkpoints now run on `SessionEnd` and `PreCompact`, and track
  a per-session byte offset so only new content is sent.
- **Hook failures left no trace.** Every run now appends one line to
  `~/.anamnesia/hooks.log`, and `doctor` reports a hook that is failing on
  every turn. A hook still never breaks a session.
- **`httpPost` only accepted HTTP 200**, so every successful ingest (202) and
  experience write (201) was classified as an error.
- **Reading stdin had no deadline**, so a hook could block indefinitely on a
  pipe that was never closed. The `os.Stdin.Stat()` error is checked too.
- **Malformed configuration was silently replaced by defaults.**
  `ANAMNESIA_EMBED_DIMS=abc` quietly became 1536. Every parse failure is now
  reported, with all of them collected into one message.
- **Configuration precedence contradicted its own documentation.** Files were
  merged after the environment, so a project file overrode an environment
  variable, and the global file overrode the project file. The order is now
  flags, environment, project file, global file, defaults.
- **The hand-rolled TOML parser mangled input**, reading keys under a
  `[section]` as top-level and folding inline comments into values. Reading
  now uses a real parser; writing edits lines so comments survive.
- **PII scrub failures were silent**, persisting unredacted content without a
  log line.
- **Changing the port half-broke the install.** The host side read a separate
  variable that nothing documented; the server URL is now derived from
  `server.addr`, and `doctor` fails when the recorded MCP URL disagrees.
- **The very first start raced Postgres's initialisation.** The postgres image
  runs a temporary server to create the database, then shuts it down and
  starts the real one. That temporary server listens on the unix socket, so
  `pg_isready` reported success against it and the server connected into a
  process that was about to exit, failing with `unexpected EOF`. Readiness is
  now checked over TCP, which the temporary server never listens on, and
  success has to repeat before it is believed.
- **A data volume outliving its config caused an unexplained auth failure.**
  `POSTGRES_PASSWORD` only applies when a data directory is first created, so
  reinstalling while keeping your memory left the old password in place.
  Anamnesia now detects this and updates the password over the container's
  trusted local socket, and explains the options if it cannot.
- `restart` no longer reports success while leaving the old process running,
  and a server that fails to boot is reported in about a second with the
  relevant log lines, rather than after a 60-second timeout.
- `start` distinguishes its own server from one it did not start, instead of
  reporting "already running" for a process that may be running different
  configuration.
- **`doctor` failed on a hook error the server's own start had already fixed.**
  Hooks fire before the server exists on every first install, so the newest
  recorded run of a verb was a failure, and doctor reported it as a fault while
  everything else passed. A failure older than the running server is now shown
  as history rather than counted against the install.
- **`update` said "Update complete" after failing to replace the binary.** When
  the install directory needs elevated permissions, the self-update is skipped
  and the rest of the installation is still reconciled, which is deliberate.
  Ending with "Update complete" made that read as a successful upgrade, and is
  how someone ends up believing they are running a version they are not. It now
  states that the binary was not updated, and which version is still installed.
- **The sudo suggestion dropped the flags that chose the release.** Someone who
  ran `update --pre` was told to run `sudo anamnesia update`, which consults the
  stable channel and would not find the prerelease they were installing. The
  suggestion now carries `--pre` and `--force` through.
- `trimLine` no longer splits multi-byte characters.
- Claude Code's config files are backed up before they are first modified.

- **`/v1/hooks` ignored `limit`.** It returned the whole tail whatever was
  asked for, so a panel wanting the last five entries was sent two hundred.
  Every other list route honours `limit`; this one now does too, rejecting a
  non-positive value the way they do and treating the tail as the ceiling
  rather than an error.

- **`restart` could leave nothing running.** `stop` sent SIGTERM and gave up
  after 15 seconds, while the server allows itself 30 to finish in-flight work.
  `/v1/activity/stream` is in flight until its client goes away, so a browser
  left open on the console held shutdown for the full budget every time. `stop`
  reported a server that would not exit, `restart` returned before it ever
  started one, and the server then exited on its own a few seconds later,
  leaving a stale pid file and no server.

  Two things were wrong. Shutdown now closes the streams instead of waiting for
  the browser: they watch for it alongside their client's disconnect. And the
  server's budget is a declared setting (`server.shutdown_wait`) rather than a
  literal in two packages, which is how the two came to disagree; `stop` waits
  it out plus a margin, whatever it is set to.

### Added

- **The server can now say what it is doing, and why.** It kept no record of
  its own reasoning: which loop ran last, what the extractor decided about a
  checkpoint, why a retrieval returned what it did. An install that was
  quietly extracting nothing looked exactly like one with nothing to extract.
  An in-memory recorder now holds one record per worker loop and a bounded
  ring of traces, served read-only at `GET /v1/activity`,
  `/v1/activity/{id}` and `/v1/activity/stream`.

  An ingest trace carries the source that arrived, the gate verdict with the
  score and reason behind it, the memories fetched as context, the model's own
  response, the operations it asked for and the rows written. A retrieve trace
  carries the query, both halves of the search, the fused ranking and what the
  reranker did to it. Consolidation and session start are traced too. Nothing
  is persisted: a restart empties it, which is what makes it cost no schema
  and no write on the hot path. `activity.enabled` turns it off, and
  `activity.traces` sizes the ring.

  The list and stream views leave the steps out, but carry `step_timings`:
  one name and duration per step, so a reader can see where the time went
  without pulling every trace in full. A step that has a model or a gate
  verdict carries that too, and nothing else.

- **Queue depth now moves on the stream.** It arrived in the opening
  snapshot and never again, because the recorder could emit `trace`, `step`
  and `loops` events and nothing else. A console tile therefore showed the
  depth at the moment the page connected for as long as it stayed open: an
  embedding finished, the worker lane said so, and the tile kept reporting
  work that was already done. There is a `queues` event now, published from
  the three places depth actually changes — a source arriving, the extractor
  draining one, the embed worker backfilling a batch — and not on a timer, so
  an idle install stays silent instead of counting rows once a second to
  report a number that has not moved. It carries the same two fields under
  the same names the snapshot uses, server-wide as the snapshot is.

- **Every worker tick now says what it did.** The six loops report "1 source,
  2 operations" or "nothing to embed" rather than only proving the process is
  alive.

- **The memory itself is readable over HTTP.** `/v1/facts`,
  `/v1/experiences`, `/v1/skills`, `/v1/entities`, `/v1/edges`, `/v1/sources`
  and `/v1/working` list and page their tables, each with a detail route and
  the filters that domain actually has. Paging is keyset rather than offset,
  so a list that is still being written to does not shift under a reader.
  Alongside them: `/v1/stats` for counts per domain including embedding
  coverage, `/v1/projects` and `/v1/users` with counts and last activity,
  `/v1/stats/activity` for writes per day, `/v1/embedding-map` for stored
  vectors projected onto two components, `/v1/config` for the resolved
  settings with secrets masked, and `/v1/hooks` for the parsed hook log.

  All of it is read-only, and it means it: these routes resolve `?user=` and
  `?project=` by lookup, so a typo is a `404` rather than a newly created user.

- **`anamnesia init` is back.** It was removed when project settings moved
  into `.anamnesia.toml`, on the grounds that `anamnesia config --project set`
  could create the file. It can, but it leaves two naked lines and no hint of
  what else a repository can override, which is a poor first thing for a new
  user to meet. `init` detects the slug from the repository directory, writes
  the file with each project-settable key documented beside it, and refuses to
  overwrite one that already exists without `--force`.

  The template is rendered from the settings table rather than written out
  here, so it cannot drift from what is settable, and secrets are excluded
  structurally: `.anamnesia.toml` is committed with the repository, and a
  generated file does not get to publish an API key. Keys it has no value for
  are written commented out, because a blank value is an override like any
  other and `identity.user = ""` would file that repository's memories under
  whoever `$USER` happens to be.

- **The forgetting policy is configurable.** The decay half-lives were
  constants nothing could reach, so the rate at which memory fades could not
  be tuned and nothing could report it. They are now
  `decay.half_life_case`, `decay.half_life_strategy` and
  `decay.half_life_hybrid`, with the values the worker was already using.

- **`anamnesia update` updates itself.** It compares the running build against
  the latest GitHub release and, when a newer one exists, downloads that
  release's binary for the platform, verifies its SHA-256 against the
  `checksums.txt` published in the same release, confirms the download runs and
  reports the version it claims to be, and only then replaces the binary
  atomically. The remainder of the update is handed to the new binary, so the
  version stamped into Claude Code's hooks and the code enforcing the schema
  are the version that will serve.

  It refuses on a checksum mismatch, a missing `checksums.txt`, a release with
  no asset for the platform, or a binary that disagrees about its own version.
  It never escalates privileges: an unwritable install directory produces the
  `sudo` instruction instead. A locally built binary reports a commit hash
  rather than a version and is not replaced without `--force`.
  `--check` reports without changing anything; `--no-self-update` reconciles
  the installed binary only.

  Updates follow stable releases only, because GitHub's "latest release"
  deliberately excludes prereleases. `--pre` opts into them per run, picking
  the highest version rather than the most recently published one, and skipping
  drafts. When only a prerelease exists, the stable channel names it instead of
  reporting that nothing is published.
- **Self-update asks for a password instead of demanding sudo.** When the
  binary lives somewhere root owns, the download and verification still happen
  as the user, and only the file swap is escalated, as a single
  `sudo install` of the already-verified file. The prompt appears only on a
  terminal, so a script prints the instruction and exits rather than hanging.
- **Running under sudo is refused.** `sudo anamnesia update` writes the user's
  config, patches Claude Code's `settings.json` and `.claude.json`, and starts
  the server: as root, every one of those becomes root-owned and the user can
  no longer write their own Claude Code files. Commands that only read
  (`doctor`, `status`, `version`) still run, a genuine root account is
  unaffected, and `--allow-root` overrides it.
- **A release workflow.** Pushing a `v*` tag runs gofmt, vet and the race
  tests, cross-compiles for macOS and Linux, writes `checksums.txt`, verifies
  the artifacts are what the updater expects, and publishes a GitHub release
  with install instructions. Tags that are not plain semver are rejected
  rather than published, because the updater could not compare them. The
  workflow can be run manually to rehearse a build without publishing.
- `anamnesia setup`: creates the config, wires Claude Code, starts the
  stack, reports health. Idempotent.
- `anamnesia start` / `stop` / `restart` / `status` / `logs`: Anamnesia
  manages its own Postgres container through the `docker` CLI.
- `anamnesia config` with dotted keys: `get`, `set`, `list`, `path`, `edit`,
  `--project` for per-repository overrides, and secret masking.
- `anamnesia update`: reconciles hooks, image, schema and server with the
  binary you are running.
- `anamnesia doctor`: eleven checks, `--json`, and `--deep` to write and read
  back a canary memory.
- `anamnesia migrate --dims N`: rebuilds the embedding columns and indexes.
- `anamnesia uninstall --purge`: also removes the container, its volume and
  `~/.anamnesia`.
- Hooks start the stack on demand when `server.autostart` is set, under a
  lock so concurrent hooks cannot race.
- Tests for the CLI, which previously had none: install idempotency, the
  duplicate-hook regression, uninstall fidelity, configuration precedence and
  validation, incremental transcript reading, and the hook log.
- CI: formatting, vet, race tests, plus a smoke job that installs, starts and
  verifies a real stack.

### Changed

- **`config list` no longer claims every setting came from the environment.**
  `anamnesia start` hands the server its own configuration as environment
  variables, so inside the server every configured value looked like an
  environment override of itself. An environment value equal to what the files
  already resolved to is now reported as coming from the file it came from.

- **The server runs as a host process, not a container.** There is no
  Anamnesia image and no compose file. `docker-compose.yml`, `Dockerfile` and
  `.env.example` are gone; `~/.anamnesia/config.toml` replaces `.env`.
- The CLI, the hooks and the server are one executable, so version skew
  between them is no longer possible.
- `server.addr` defaults to `127.0.0.1:8181` rather than all interfaces.
- The LLM HTTP timeout is a normal setting (`llm.timeout`) instead of an
  environment variable read directly inside the LLM client.
- Removed `anamnesia up` and `down`; the stack is `start` and `stop`. Project
  settings live in `.anamnesia.toml`, written by `anamnesia init` or one key
  at a time with `anamnesia config --project set`.
- Removed the unused `ANAMNESIA_WORKER_IN_PROCESS` setting, which was parsed
  and documented but never read.
- The extractor now asks the model to name an experience, not only describe
  it. `ADD_EXPERIENCE` arrived without a title often enough that a list of
  memories read as a column of body text, and an untitled experience is a
  weaker search target: `experiences.title` feeds the tsvector lexical
  retrieval uses.
