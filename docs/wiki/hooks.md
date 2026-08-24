# Hooks

Anamnesia adds seven hooks to Claude Code's `~/.claude/settings.json`. They
are the only thing that makes memory automatic; everything else is a surface
you have to call.

## The seven

| Claude Code event | Verb | Budget | What it does |
|---|---|---|---|
| `SessionStart` | `session-start` | 6s | Loads your facts and recent experiences into the session |
| `UserPromptSubmit` | `retrieve` | 2.5s | Retrieves what is relevant to this prompt |
| `PreCompact` | `pre-compact` | 20s | Checkpoints before context is compacted |
| `SessionEnd` | `session-end` | 20s | Checkpoints when the session ends |
| `Stop` | `flush` | 20s | Checkpoints mid-session once enough has accumulated |
| `SubagentStop` | `subagent-stop` | 8s | Records what a subagent concluded |
| `PostToolUse` | `artifact` | 8s | Records a published artifact, matched to the `Artifact` tool |

The budgets differ because the cost of being slow differs. `retrieve` is on
the critical path of every prompt you type, so it gets 2.5 seconds.
`session-end` has nobody waiting, so it gets twenty.

You can run any of them by hand, which is what the timeouts are measured
against:

```bash
echo '{"session_id":"…","transcript_path":"…"}' | anamnesia hook session-start
```

## Three rules that are not negotiable

**Hooks never break a session.** They exit 0 whatever happens: server down,
config unreadable, model outage, timeout. A memory layer that can wedge your
editor is worse than no memory layer.

**Every run is recorded** in `~/.anamnesia/hooks.log`, one line per run with
the verb, duration and outcome. This is the other half of the rule above: if
hooks exit 0 on failure, something has to notice that they have been failing
every turn for a week. `anamnesia doctor` reads this file and tells you.

**Hooks are written with the absolute binary path.** The shell Claude Code
spawns often does not have `/usr/local/bin` on its `PATH`. Move the binary and
re-run `anamnesia install`.

## Incremental checkpoints

Each session has a byte offset in `~/.anamnesia/offsets/`, recording how far
its transcript has been read. A checkpoint sends only what was added since the
last one.

This is why a long session costs no more than a short one, and it is what
makes mid-session flushing affordable at all. Ten flushes across a session
send the same bytes as one at the end, cut the same way, plus one trailing
partial segment each.

## The Stop hook, and why it is gated

`Stop` fires after **every assistant turn**. It was removed once, and is back
gated. Understanding why matters before touching it.

It was retired because it re-sent the whole transcript each time, so ingest
grew with the square of the session length. The offset above fixed that. What
still must not happen is flushing on every turn, which is what the two gates
prevent:

- **`ingest.flush_bytes`** (16384) checkpoints once that many new bytes have
  accumulated. Bytes rather than turns, because bytes are what line up with
  segments: reaching the threshold means there is a segment's worth of new
  material to cut, so a flush produces whole segments instead of slivers.
- **`ingest.flush_after`** (20m) checkpoints once that long has passed since
  the last one, however little accumulated. The backstop for a slow
  conversation that should not sit uncheckpointed for hours.

Set both to 0 to checkpoint only at `PreCompact` and `SessionEnd`, which is
what earlier versions did.

## Segmentation

A checkpoint is cut into segments before it is sent, so the surprise gate
judges one subject at a time rather than a whole session.

- **`ingest.segment_gap`** (20m): a pause longer than this starts a new
  segment.
- **`ingest.segment_max_bytes`** (4000): a segment is cut when it grows past
  this, because a long unbroken session is still not one idea.

That second number is measured, not guessed. On three real 21KB sessions the
same content yielded **14 unique facts at 32768 and 74 at 4000**, and the ones
only the smaller cap found were standing preferences like "branch instead of
committing directly to main", which is exactly what memory is for. Attention
degrades over a long input, so a bigger segment does not mean more extracted,
it means less. The cost is one model call per segment: 3 calls became 18.

Set either to 0 to disable that cut.

## Recovery

A session that crashes fires neither `PreCompact` nor `SessionEnd`, so its
last stretch is never checkpointed.

It is not lost. The transcript is on disk and the offset says how far it was
read. `anamnesia recover` collects those tails, and `SessionStart` spawns it
detached so it costs the new session nothing.

```bash
anamnesia recover        # by hand, if you want to watch it
```

`ingest.recover_idle` (15m) is how long a transcript must go unwritten before
recovery treats its session as over. It is the only judgement recovery makes,
and it cuts both ways: too short and it ingests a live session's tail, racing
that session's own checkpoint and paying to extract content about to be sent
again; too long and a crashed session's work sits uncollected. Nothing is lost
either way, because the transcript stays on disk until recovery reads it.

Set `ingest.recover_stranded` to false to leave abandoned tails alone.

## Autostart

`server.autostart` (true) lets a hook start the stack when it is not running,
so a new session heals itself instead of silently losing memory.
`~/.anamnesia/start.lock` guards against two hooks racing to start it.

## What the artifact hook does differently

`PostToolUse` is matched to the `Artifact` tool and takes a different path
from every other hook: no source row, no surprise gate, no model. It parses
the tool's own response for the URL and uuid, reads the published file while
it still exists, and writes an `artifacts` row directly.

See [Memory model](memory-model.md#artifacts) for why.

## Reading the log

```bash
tail -f ~/.anamnesia/hooks.log
anamnesia doctor            # summarises it, including "no hook has run yet"
```

A hook that fails every run shows up in `doctor` as a failing check, with the
error it has been swallowing.

## Installing and removing

```bash
anamnesia install          # (re)wire hooks and MCP only
anamnesia uninstall        # remove exactly Anamnesia's entries
```

`install` owns **any** hook entry that runs `anamnesia hook`, not only entries
carrying the `_anamnesia_managed` marker. Keying off the marker alone appended
a second copy of every hook for anyone upgrading from a version that predated
it.

Both back up the files they touch before first writing them, and `uninstall`
leaves everything that is not Anamnesia's alone.
