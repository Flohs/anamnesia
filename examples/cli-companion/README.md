# cli-companion — reference dock-on agent

A minimal Go CLI that demonstrates how an external agent "docks onto"
Anamnesia through the public MCP surface only. It's the proof that
Anamnesia is a substrate, not just a Claude Code add-on.

## What it does

**Boot**
- `anamnesia_identity` → loads the user's persona as the system prompt.
- `anamnesia_capabilities` → discovers registered skills/tools.

**Per turn**
- `anamnesia_search` → pulls top-K context for the prompt.
- Sends persona + context + prompt to Claude (Anthropic Messages API,
  raw HTTP). With no `ANTHROPIC_API_KEY` it returns a stub reply so the
  contract is still exercisable offline.
- If the reply contains future-tense commitment language, records it via
  `anamnesia_commitments_record`.

**On exit (^D)**
- `anamnesia_ingest` → pushes the whole transcript so the kernel's
  extractor decides what's worth keeping.

## The constraint

This is a **separate Go module**. It imports nothing from
`github.com/flohs/anamnesia-open-source/internal/*` — the MCP surface is
the only contract. Enforced in CI:

```bash
go list -deps ./... | grep '^github.com/flohs/anamnesia-open-source/internal/'
# must print nothing
```

If you need a primitive that isn't on the MCP surface, add it to the
surface — don't reach into internals. That discipline is the whole point:
any agent in any language that speaks MCP can dock the same way.

## Run

```bash
# 1. Anamnesia must be up:
(cd ../.. && ./bin/anamnesia up)

# 2. Optionally seed a persona so the agent has a voice:
curl -sS http://localhost:8181/mcp/ -H 'content-type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"anamnesia_facts_upsert","arguments":{"key":"user.persona.system_prompt","value":{"v":"Be terse and concrete."},"scope":"user"}}}'

# 3. Run the companion:
export ANTHROPIC_API_KEY=sk-ant-...   # optional; omit for stub replies
go run . -user=default
```
