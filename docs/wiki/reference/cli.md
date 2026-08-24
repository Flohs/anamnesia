# CLI reference

Every command. Mirrors the cobra definitions in `cmd/anamnesia/`.

`anamnesia <command> --help` is authoritative and always current.

## Global flags

| Flag | Effect |
|---|---|
| `-v`, `-vv` | Increase logging verbosity |
| `--config-dir` | Override the config directory |
| `--allow-root` | Permit running under sudo, with the warning it prints |

**Do not run `sudo anamnesia`.** It is refused for anything that writes your
config, Claude Code's files or the server's pid file, because under sudo those
end up owned by root and you can no longer write your own files. `version`,
`help`, `doctor`, `status` and completion are the exceptions.

---

## Setting up

### `anamnesia setup`
Create the config, wire Claude Code, and start the stack.

The whole installation in one command, and idempotent: run it again any time
to repair what has drifted without touching your settings.

| Flag | Effect |
|---|---|
| `--no-hooks` | Skip patching Claude Code's config |
| `--no-start` | Skip starting the stack |
| `--no-completion` | Do not install shell completion or touch your rc file |
| `--no-pull` | Skip pulling the postgres image |

### `anamnesia init`
Write `.anamnesia.toml` for this repository.

Detects the slug from the directory and refuses to overwrite an existing file.
The result is meant to be committed, so it never holds secrets.

| Flag | Effect |
|---|---|
| `--project` | Project slug to write. Default: this directory's name |
| `--force` | Rewrite an existing `.anamnesia.toml` |
| `--dry-run` | Print the resulting file instead of writing it |

### `anamnesia install`
Wire Claude Code's hooks and MCP config to Anamnesia.

Owns any hook entry that runs `anamnesia hook`, not only ones it marked, which
is what stops an upgrade appending a second copy of every hook.

| Flag | Effect |
|---|---|
| `--scope` | `user` (`~/.claude`) or `project` (`$PWD/.claude`) |
| `--no-completion` | Leave your shell rc file alone |

### `anamnesia uninstall`
Remove Anamnesia's entries from Claude Code's config.

Removes exactly its own entries and leaves the rest alone.

| Flag | Effect |
|---|---|
| `--purge` | Also remove the postgres container, its volume, and `~/.anamnesia` |
| `--scope` | Which hook scope to clean |

`--purge` deletes your memory. That is what it is for.

---

## Running the stack

### `anamnesia start`
Start the local stack: postgres container and server.

### `anamnesia stop`
Stop the server.

| Flag | Effect |
|---|---|
| `--all` | Also stop the postgres container |

### `anamnesia restart`
Restart the server. What you run after changing a setting.

### `anamnesia status`
Show whether the stack is running.

| Flag | Effect |
|---|---|
| `--json` | Print machine-readable status |

### `anamnesia logs`
Show the server log.

| Flag | Effect |
|---|---|
| `-f` | Follow |
| `-n` | How many lines |

---

## Configuration

### `anamnesia config`
Read and write settings. With no arguments, shows every setting, its value and
where it came from.

| Flag | Effect |
|---|---|
| `--global` | Use `~/.anamnesia/config.toml` (default) |
| `--project` | Use `./.anamnesia.toml` for this repository |
| `--show-secrets` | Print API keys and passwords in full |

**Subcommands:**

| Command | Effect |
|---|---|
| `config get <key>` | Print one resolved value |
| `config set <key> <value>` | Write one value to the config file |
| `config list` | Show every setting, its value and where it came from |
| `config path` | Print the config file location |
| `config edit` | Open the config file in `$EDITOR` |

Invalid values are rejected at `set` time, naming the setting. See
[Configuring](../configuration.md).

---

## Verifying

### `anamnesia doctor`
Verify the installation and report what is wrong. Exits non-zero on failure,
so it works in a script.

| Flag | Effect |
|---|---|
| `--deep` | Also write and read back a canary memory |
| `--json` | Emit the report as JSON |
| `--scope` | Which hook scope to inspect |

`--deep` exercises the embedder, which no other check does. Run it after
changing providers.

### `anamnesia version`
Print version. A locally built binary reports a commit hash rather than a
version, which is what stops `update` from silently replacing your build.

---

## Memory

### `anamnesia artifacts`
List the artifacts Claude Code has published.

| Flag | Effect |
|---|---|
| `--limit` | How many to show |
| `--project` | Project slug. Default: this repository |
| `--user` | User handle. Default: from your config |
| `--json` | Machine-readable |

**Subcommand `artifacts backfill`** recovers artifacts from transcripts that
predate the hook. Idempotent, so it doubles as the repair path for anything
missed while the server was down.

| Flag | Effect |
|---|---|
| `--all` | Scan every project, not only this one |
| `--dry-run` | Report what would be recorded and change nothing |

### `anamnesia recover`
Ingest transcript tails from sessions that ended without a checkpoint.

`SessionStart` spawns this detached, so it usually runs on its own.

| Flag | Effect |
|---|---|
| `--dry-run` | Report what would be ingested without ingesting it |

### `anamnesia project`
Inspect and reorganise project scopes.

| Command | Effect |
|---|---|
| `project move <to>` | Refile this repository's memories under another project |
| `project prune` | Remove project entries that hold no memories |

Both default to reporting only. Pass `--apply` to carry it out.

| Flag | Effect |
|---|---|
| `--apply` | Carry the change out instead of only reporting it |
| `--from` | Move this project instead of the one this directory resolves to |

---

## Database

### `anamnesia migrate`
Apply database migrations and exit.

| Flag | Effect |
|---|---|
| `--dims N` | Rebuild the embedding columns at this width |

`--dims` discards stored vectors so they can be re-embedded. Facts and
experiences are not lost. Migration is serialised by a Postgres advisory lock,
so running it while the server boots is safe.

---

## Updating

### `anamnesia update`
Update Anamnesia and reconcile the installation with it.

Compares against the latest GitHub release, verifies the SHA-256, confirms the
downloaded binary runs, and only then replaces itself. The rest of the update
is handed to the new binary.

| Flag | Effect |
|---|---|
| `--check` | Only report whether a newer release exists |
| `--pre` | Consider prereleases as well as stable releases |
| `--no-self-update` | Reconcile this binary, download nothing |
| `--force` | Download the latest release even when this is not a released build |
| `--no-hooks` | Skip refreshing Claude Code's config |

If the binary lives somewhere you do not own, `update` downloads and verifies
as you, then asks before escalating the single install step. On a
non-interactive terminal it prints the instruction and exits rather than
hanging on a password.

---

## Measurement

### `anamnesia eval`
Measure retrieval against the built-in fixture corpus.

| Flag | Effect |
|---|---|
| `--json` | Print the report as JSON |
| `--baseline` | Compare against a previous `--json` report and fail on regression |
| `--k` | How many hits to request per query |
| `--keep` | Leave the ingested corpus in place |

For LongMemEval, see [Retrieval](../retrieval.md#running-it-yourself).

---

## Internal

Hidden from `--help`. Invoked by Claude Code and by `anamnesia start`.

### `anamnesia hook <event>`
Run a Claude Code hook. Events: `session-start`, `retrieve`, `session-end`,
`pre-compact`, `flush`, `subagent-stop`, `artifact`.

Reads a JSON payload on stdin. Always exits 0. See [Hooks](../hooks.md).

### `anamnesia serve`
Run the HTTP API, MCP endpoint and background worker.

| Flag | Effect |
|---|---|
| `--worker` | Run only the background worker |
| `--no-worker` | Skip the background worker (HTTP only) |

Refuses to boot when `embed.dims` and the schema disagree.
