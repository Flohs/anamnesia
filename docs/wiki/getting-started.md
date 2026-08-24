# Getting started

From nothing to a working install, with the detail the README leaves out.

## Requirements

- **Docker**, for the Postgres container. Docker Desktop, OrbStack, colima and
  podman's docker shim all work.
- **Claude Code**, the client Anamnesia wires itself into.
- **macOS or Linux.**
- Optionally an **API key** for a model. Without one everything runs, but
  extraction produces nothing: you get the plumbing and an empty memory.

Building from source additionally needs **Go 1.25.7+**.

## 1. Install the binary

Download from the
[latest release](https://github.com/Flohs/anamnesia/releases/latest), verify
the checksum, and put it somewhere permanent. Swap the asset name for your
platform: `anamnesia-darwin-arm64`, `anamnesia-darwin-amd64`,
`anamnesia-linux-amd64` or `anamnesia-linux-arm64`.

```bash
REPO=https://github.com/Flohs/anamnesia/releases/latest/download
curl -fsSLO $REPO/anamnesia-darwin-arm64
curl -fsSLO $REPO/checksums.txt
shasum -a 256 --check --ignore-missing checksums.txt
sudo install -m 0755 anamnesia-darwin-arm64 /usr/local/bin/anamnesia
```

Or build it:

```bash
git clone https://github.com/Flohs/anamnesia.git
cd anamnesia
sudo make install          # builds, then installs /usr/local/bin/anamnesia
```

**Permanent matters.** The hooks are written with the absolute path of the
binary you install, because the shell Claude Code spawns often does not have
`/usr/local/bin` on its `PATH`. Move the binary later and the hooks point at
nothing until you re-run `anamnesia install`.

`make build` alone leaves it at `./bin/anamnesia` if you would rather place it
yourself.

## 2. Run setup

```bash
anamnesia setup
```

That is the whole installation. In order, it:

1. writes `~/.anamnesia/config.toml` with a generated database password
2. patches `~/.claude/settings.json` with seven hook entries
3. patches `~/.claude.json` with the MCP server endpoint
4. writes a completion script and sources it from your shell's rc file
5. pulls `pgvector/pgvector:pg16` and creates the container
6. waits for Postgres, applies migrations, starts the server
7. prints what it did and whether it worked

`setup` is idempotent. Run it again whenever you like: it repairs what has
drifted and leaves your settings alone.

Useful flags when you want less than all of it:

```bash
anamnesia setup --no-hooks        # config and stack only, do not touch Claude Code
anamnesia setup --no-start        # write config and wiring, start nothing
anamnesia setup --no-completion   # leave your shell rc file alone
```

## 3. Give it a model

`embed stub` in setup's final line means nothing will be extracted. One
OpenRouter key covers all three workloads:

```bash
anamnesia config set openrouter.api_key sk-or-v1-…
anamnesia restart
```

Direct provider keys work too:

```bash
anamnesia config set anthropic.api_key sk-ant-…
anamnesia config set llm.provider anthropic
anamnesia config set openai.api_key sk-…      # for embeddings
anamnesia config set embed.provider openai
anamnesia restart
```

The restart matters: the server reads its providers at boot.

**Check that `embed.dims` matches your embedding model.** The default 1536
is right for `text-embedding-3-small`. If you pick `-3-large` you need 3072,
and changing the setting alone is not enough, because the database columns
are already the old width. See
[Troubleshooting](troubleshooting.md#the-server-refuses-to-start-and-mentions-vectorn).

## 4. Verify

```bash
anamnesia doctor
```

```
  [ ok ] config      ~/.anamnesia/config.toml
  [ ok ] binary      /usr/local/bin/anamnesia (0.2.0)
  [ ok ] docker      engine 29.4.0
  [ ok ] postgres    container anamnesia-postgres running on 127.0.0.1:5434
  [ ok ] server      responding on http://127.0.0.1:8181
  [ ok ] schema      v11, vector(1536), database ok
  [ ok ] queue       0 sources awaiting extraction, 0 rows awaiting embedding
  [ ok ] hooks       7 entries in ~/.claude/settings.json
  [ ok ] mcp         http://127.0.0.1:8181/mcp
  [ ok ] completion  ~/.anamnesia/completions/anamnesia.zsh
  [warn] hook runs   no hook has run yet
                     → start a Claude Code session, then re-run this
```

Every failure names the command that fixes it. `doctor` exits non-zero when
any check fails, so it works in a script.

`anamnesia doctor --deep` additionally writes a memory and reads it back,
which exercises the exact path your sessions use, including the embedder. It
is the check to run after changing providers.

## 5. Use Claude Code

Restart Claude Code, because it reads hooks and MCP servers at startup. Then
work normally.

Memory accumulates from your sessions, so the first one has nothing to recall
and later ones do. The `hook runs` warning clears once a session has run.

To watch it happening:

```bash
tail -f ~/.anamnesia/hooks.log     # one line per hook run
anamnesia logs -f                  # the server, including extraction
```

## Per repository

Memories are filed under a project slug, defaulting to the git repository's
directory name. Pin it for one repository:

```bash
anamnesia init
```

That writes `.anamnesia.toml` at the repository root, with the slug detected
from the directory and the other per-project settings documented alongside.
It refuses to overwrite a file that already exists.

The file is meant to be committed, so it never holds API keys or passwords.
Settings marked project-scoped in the [config reference](reference/config.md)
are the ones that can appear in it.

## Per person

For a small team sharing one server, give each person a distinct
`identity.user`. Memory partitions by user automatically, with no further
configuration.

## Next

- [Configuring](configuration.md), how values resolve and where to put them
- [Hooks](hooks.md), what fires when and what it sends
- [Troubleshooting](troubleshooting.md), when the above did not go this way
