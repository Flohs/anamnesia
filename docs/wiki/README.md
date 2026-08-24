# Anamnesia wiki

Documentation for Anamnesia: local-first long-term memory for Claude Code.

The [README](../../README.md) tells you what this is and gets it running.
These pages are what comes after that.

## Start here

| I want to | Read |
|---|---|
| Install it and get a first session working | [Getting started](getting-started.md) |
| Understand a setting, or where a value came from | [Configuring](configuration.md), then [config reference](reference/config.md) |
| Know what it keeps and what it throws away | [Memory model](memory-model.md) |
| Know when it writes and what it sends | [Hooks](hooks.md) |
| Know how it decides what is worth keeping | [Extraction](extraction.md) |
| Know how it finds things again | [Retrieval](retrieval.md) |
| Fix something that is broken | [Troubleshooting](troubleshooting.md) |
| Work on the code | [Architecture](architecture.md) |

## Reference

Complete listings, one page each:

- [Configuration](reference/config.md), all 55 settings with defaults and environment variables
- [CLI](reference/cli.md), every command and subcommand
- [MCP tools](reference/mcp-tools.md), the 24 tools Claude can call
- [HTTP API](reference/http-api.md), the 28 routes the server serves

## The short version

One Go binary is the CLI, the Claude Code hooks and the memory server, so
those three can never be on different versions. It manages one Postgres
container through the `docker` CLI. There is no compose file and no Anamnesia
image.

Seven hooks watch a Claude Code session. Checkpoints land as rows in
`sources`. A background extractor reads them, runs a surprise gate, asks a
model what is worth keeping, and writes typed memory. The default answer is
`NOOP`, because most conversation is noise. Raw content expires after seven
days.

Retrieval fuses vector similarity, full-text search and a graph walk, and
hands the result back at the start of your next session.

Nothing leaves your machine except the model calls you configure.

## Related documents

- [SECURITY.md](../../SECURITY.md), what is stored and what leaves the machine
- [CONTRIBUTING.md](../../CONTRIBUTING.md), working against a throwaway install
- [CLAUDE.md](../../CLAUDE.md), instructions for an agent working on this codebase
- [The LongMemEval baseline](../longmemeval-retrieval-baseline.md), the full measurement record
