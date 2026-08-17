# Contributing

Thanks for taking a look. Anamnesia is a single Go module with no build
tooling beyond the Go toolchain and Docker.

## Getting set up

```bash
git clone https://github.com/Flohs/anamnesia-open-source.git
cd anamnesia-open-source
make build           # ./bin/anamnesia
make lint            # gofmt, vet, tests: exactly what CI runs
```

Work against a throwaway install rather than your own, by pointing
`ANAMNESIA_HOME` somewhere temporary and giving the container its own name
and port:

```bash
export ANAMNESIA_HOME=/tmp/anamnesia-dev
./bin/anamnesia setup --no-hooks --no-start
./bin/anamnesia config set postgres.container anamnesia-dev
./bin/anamnesia config set postgres.volume anamnesia-dev-data
./bin/anamnesia config set postgres.port 5435
./bin/anamnesia config set server.addr 127.0.0.1:8182
./bin/anamnesia start
./bin/anamnesia doctor --deep
```

`--config-dir` on `install`, `uninstall` and `doctor` points the Claude Code
patching at a scratch directory, so you can exercise the real code paths
without touching `~/.claude`.

Clean up with `./bin/anamnesia uninstall --purge`.

## What to keep in mind

**The install and update paths carry the most risk.** They edit files that
belong to the user and to Claude Code. A mistake there is invisible until
memory has already been lost or duplicated, so those paths have tests and
should keep them: see `cmd/anamnesia/install_test.go`.

**Prefer failing loudly over defaulting quietly.** Most of the bugs this
project has had were silent: a config value that fell back to a default, a
health check that could not fail, a hook that swallowed its error. If a value
is wrong, say which one and what to do about it.

**Hooks must never break a session.** They run inside someone's editor. They
exit 0 whatever happens, and record what happened in
`~/.anamnesia/hooks.log` so `doctor` can report it.

**One place per setting.** Everything configurable is declared once in
`cmd/anamnesia/settings.go`, which generates the config file, validates
input, and renders the server's environment. Adding a setting anywhere else
reintroduces the drift that table exists to prevent.

## Adding a setting

1. Add one entry to `settings` in `cmd/anamnesia/settings.go`, with its
   default, its documentation, and the `ANAMNESIA_*` variable it maps to (or
   none, if it is host-side only).
2. Read it from `internal/config` if the server needs it.
3. Nothing else. The config file, `anamnesia config`, validation and the
   server environment all follow from the table.

## Database changes

Migrations are embedded SQL in `internal/store/migrations`, applied by goose
on server start. Add a new numbered file; never edit an applied one.

Anything that touches the embedding columns has to keep the schema and
`embed.dims` in agreement, because a mismatch makes every embedding write
fail. `SetEmbeddingDims` and the boot-time check in `serve.go` are what
enforce that.

## Pull requests

- `make lint` passes.
- New behaviour has a test, especially in `cmd/anamnesia`.
- Commits explain why, not just what.
- Say what you actually verified. "Tests pass" and "I ran it and it worked"
  are different claims.
