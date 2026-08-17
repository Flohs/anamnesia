# Security

## Reporting

Please report vulnerabilities privately via GitHub's "Report a vulnerability"
button on the Security tab, rather than opening a public issue.

## What Anamnesia stores, and where

Anamnesia is designed to run on one machine and hold the contents of your
working conversations. Assume the database contains everything you have
discussed with Claude Code in the projects it is wired into.

- **Memory** lives in the Docker volume `anamnesia-pgdata`, unencrypted, as
  ordinary Postgres data.
- **Raw ingested content** in `sources` expires after seven days. Extracted
  facts and experiences persist until you delete them.
- **Configuration** lives in `~/.anamnesia/config.toml` at mode `0600`,
  including your API keys and the generated database password.
- **Logs** in `~/.anamnesia/` contain conversation-derived metadata (session
  ids, byte counts, error text), not conversation content.

## Defaults

- The server binds `127.0.0.1:8181`. Postgres publishes to `127.0.0.1` only.
- The database password is randomly generated per install.
- Authentication is off by default, which is appropriate for a loopback-only
  listener: any local process running as you can already read the config file
  and the database.

If you change `server.addr` to anything other than loopback, set
`server.token` as well. `anamnesia doctor` warns when you have done one
without the other, but nothing stops you.

## PII handling

An in-process regex detector runs over content before it is stored, and can
either tag the categories it finds (`pii.mode = tag`, the default) or replace
the matches (`redact`). Microsoft Presidio is supported as a sidecar for
better detection.

This reduces exposure; it does not eliminate it. A regex detector misses
things, and `tag` mode stores the original text by design. Treat the database
as sensitive regardless of these settings.

## Third parties

With a model provider configured, ingested content is sent to that provider
for extraction, and text is sent for embedding and reranking. Which provider
is entirely your choice, including a local OpenAI-compatible endpoint via
`openai.base_url`. With no provider configured, nothing leaves your machine
and nothing is extracted.

## What Anamnesia touches on your system

- `~/.anamnesia/`: its own state.
- `~/.claude/settings.json` and `~/.claude.json`: the hook and MCP entries,
  backed up before first modification. `anamnesia uninstall` removes them.
- One Docker container and one volume, both named in your config. Anamnesia
  never touches a container or volume it was not told about.
