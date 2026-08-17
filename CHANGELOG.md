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
- `trimLine` no longer splits multi-byte characters.
- Claude Code's config files are backed up before they are first modified.

### Added

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

- **The server runs as a host process, not a container.** There is no
  Anamnesia image and no compose file. `docker-compose.yml`, `Dockerfile` and
  `.env.example` are gone; `~/.anamnesia/config.toml` replaces `.env`.
- The CLI, the hooks and the server are one executable, so version skew
  between them is no longer possible.
- `server.addr` defaults to `127.0.0.1:8181` rather than all interfaces.
- The LLM HTTP timeout is a normal setting (`llm.timeout`) instead of an
  environment variable read directly inside the LLM client.
- Removed `anamnesia init`, `up` and `down`. Project settings live in
  `.anamnesia.toml` via `anamnesia config --project`; the stack is `start`
  and `stop`.
- Removed the unused `ANAMNESIA_WORKER_IN_PROCESS` setting, which was parsed
  and documented but never read.
