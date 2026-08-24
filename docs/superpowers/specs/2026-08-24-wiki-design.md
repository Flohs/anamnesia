# Design: a wiki in the repo that explains the app

Written 2026-08-24 against `v0.1.0-rc15` (commit 3f6698a).

The ask: documentation for Anamnesia, in this repository, explaining what the
app is and how to run it. Markdown, readable on GitHub, no new hosting.

This is not the shelved `docs/wiki-layer-plan.md`, which proposes rendering a
user's *memories* as a markdown vault. Different subject, same word. That plan
stays where it is.

---

## 1. Why this needs a design and not just an afternoon of writing

The README is 596 lines and already carries install, config, commands,
artifacts, completion, updating, troubleshooting, how-it-works, the
measurement, the file layout, and development. It is the front door doing the
job of a manual.

The surface it is trying to cover, counted today:

| Surface | Count | Declared in |
|---|---|---|
| Settings | 55 | `cmd/anamnesia/settings.go` |
| CLI commands and subcommands | 29 | `cmd/anamnesia/*.go`, cobra |
| MCP tools | 24 | `internal/mcp/server.go` |
| HTTP routes | 28 | `internal/httpapi/server.go` |
| Migrations | 11 | `internal/store/migrations/` |
| Hooks | 7 | `cmd/anamnesia/hook.go` |
| Memory domains | 6 | `pkg/anamnesia/types.go` |

Write all of that by hand into a wiki and it starts accurate and ends wrong.
This project has the receipts on that failure already. From the header of
`cmd/anamnesia/settings.go`:

> Adding a setting in one place therefore cannot drift from its documentation,
> its default, or the server that consumes it, which is what previously let
> docker-compose.yml, .env.example and internal/config disagree with each
> other about what existed and what the defaults were.

Three files disagreed about the settings. The fix was to make one table the
only source and generate the rest. A hand-written wiki page listing 55
settings is that same bug, reintroduced, in a place where nothing compiles and
nothing fails.

So the design question is not "what pages do we write". It is **which pages a
human writes and which the build writes**, and how we find out when the second
kind goes stale.

## 2. The split

Every page falls in exactly one of two buckets.

**Generated.** Anything that mirrors a declaration in the code: the settings
table, the command tree, the MCP tools, the route list. Written by a
generator, checked into git, and verified by a test that regenerates and
diffs. A human never edits these; the file says so at the top.

**Hand-written.** Anything that explains *why*: what the surprise gate is for,
why checkpoints carry an offset, why the lexical channel stays despite earning
nothing, what to do when `doctor` is red. No generator can write these, and no
test can check them. They rot slowly and visibly, unlike a stale default,
which rots instantly and silently.

The rule: **if the wiki states a fact that also exists in Go, the wiki does
not get to type it.**

## 3. The drift gate

Generation alone is not the point. A generator nobody runs produces exactly
the stale page it was meant to prevent.

`TestWikiReferenceIsCurrent` regenerates every reference page into a buffer and
compares it against the committed file. Mismatch fails the test, naming the
page and the diff. Adding a setting without running the generator turns CI red
on the commit that adds the setting.

This follows the invariant CLAUDE.md already states:

> **A gate that cannot fail is not a gate.** Before trusting any green check,
> ask what it would take for it to go red.

What turns this one red: adding, removing, or re-documenting a setting; adding
or renaming a command or flag; adding or re-describing an MCP tool; adding a
route. That is the full set of ways the reference can go wrong, and each of
them trips it.

It also gives the existing invariants a second enforcement point at no cost.
`embeddingTables` and `projectScopedTables` have tests that introspect the
schema. The settings table gets the same treatment for its documentation.

### The HTTP page is a partial case, and the design says so

Settings, commands and MCP tools each carry their own prose in the
declaration: `setting.Doc`, cobra's `Short`/`Long`, and
`mcp.WithDescription`. Those three pages generate whole.

Routes do not. `mux.Handle("/v1/retrieve", d.protect(...))` carries a path and
whether it is protected, and nothing else. So `reference/http-api.md` is
hand-written prose, and what the generator contributes is the **completeness
check**: it reads the 28 registered routes and asserts every one has a section
in the page, and that the page documents no route that is not registered. An
undocumented endpoint fails the test; the wording of the documented ones is
ours.

Same gate, narrower claim. Worth being explicit that we know the difference,
because a page half under a generator's control is exactly where someone later
assumes the whole thing is checked.

## 4. Page inventory

```
docs/wiki/
  README.md              index; what to read for which question
  getting-started.md     requirements, install, setup, first session
  configuration.md       resolution order, project vs global, secrets
  memory-model.md        the six domains, and what lands in each
  hooks.md               the seven hooks, offsets, recovery, the Stop story
  extraction.md          surprise gate, candidates, the five operations
  retrieval.md           RRF fusion, decay, rerank, the lexical finding
  troubleshooting.md     doctor, health, the failures we have actually seen
  architecture.md        one binary three roles, the container, the invariants
  reference/
    config.md            GENERATED from settings.go
    cli.md               GENERATED from cobra
    mcp-tools.md         GENERATED from the MCP registry
    http-api.md          hand-written prose, generated completeness check
```

Nine hand-written pages, three generated, one checked. No page is speculative:
each maps to a section the README currently carries or a subject the README
declines to cover because it would double its length.

Ordering inside `README.md` is by question, not by architecture: "I want to
install it", "it is not working", "what does this setting do", "how does it
decide what to remember". People arrive at documentation with a question, not
a desire to tour a system.

## 5. Where the README stops

The README keeps the job it is good at and sheds the job it is bad at.

**Stays:** what Anamnesia is, what makes it different, requirements, the
four-step getting started, the measurement, the license. A visitor who reads
only the README should be able to decide whether they want this and get it
running.

**Moves to the wiki, leaving a link:** the full command list, the full
configuration surface, artifacts, tab completion, updating, the long
troubleshooting section, how it works in depth, what lives where.

Estimated README after: around 250 lines. The wiki is not a place to put more
words; it is a place to put the words that are already there and outgrowing
their container.

`CLAUDE.md` is untouched. It is instructions for an agent working on this
codebase, not documentation for a person running it, and the two have
different readers and different lifetimes. Where they overlap (the invariants)
the wiki links to CLAUDE.md rather than restating it, for the same reason the
reference pages are generated.

## 6. The generator

One command, `anamnesia docs generate`, hidden from help like `hook` and
`serve` are, writing into `docs/wiki/reference/`. It lives in
`cmd/anamnesia/docs.go` because it needs the settings table and the cobra root
command, both of which are `package main`.

For the CLI page it uses `cobra/doc`, which already renders a command tree to
markdown. For settings and MCP tools it is a template over a slice: both are
ordered tables with the prose attached.

`make lint` runs it and fails on a dirty tree, so the local loop catches drift
before CI does.

Estimated size: 150 lines of generator, 120 of test.

## 7. Non-goals

Recorded so they are not re-litigated:

- **No site generator, no hosting, no GitHub Pages.** Markdown in the repo,
  rendered by GitHub. Adding a build step to documentation is how
  documentation stops getting written.
- **No GitHub Wiki.** It is a separate repository with separate history, which
  puts the docs outside the diff of the commit that changes the behaviour they
  describe. In-repo means a change and its documentation land in one review.
- **No versioned documentation.** One version, the one on `main`. Prior
  versions are in git.
- **No API reference generated from Go doc comments.** `internal/` is not a
  public surface, and `pkg/anamnesia` is types rather than behaviour.
- **No tutorials or cookbooks in v1.** Write them when a real question repeats,
  not in anticipation of one.

## 8. Success criteria

1. `docs/wiki/` exists with the thirteen pages in section 4.
2. `anamnesia docs generate` produces the four reference pages.
3. `TestWikiReferenceIsCurrent` passes, and fails when a setting is added
   without regenerating. Both directions demonstrated in the test.
4. Every one of the 28 registered routes has a section in
   `reference/http-api.md`, enforced by that test.
5. The README is shorter, links into the wiki, and still gets a new user from
   nothing to a working install without leaving the page.
6. No hand-written page states a default, a flag name, or a tool name that the
   generator also emits.

## 9. Open questions

1. `docs/wiki/` or `docs/`? The `docs/superpowers/` tree already lives under
   `docs/`, so a flat `docs/` would mix manual pages with design specs.
   *Lean: `docs/wiki/`, matching the ask's own word.*
2. Does `make lint` regenerating count as the gate, or does CI need its own
   step? *Lean: both run the same test; the test is the gate, `make lint` is
   the fast path to it.*
3. Should `troubleshooting.md` absorb `doctor`'s failure messages by
   generation too? They are strings in `doctor.go` with real remedies attached.
   *Lean: not in v1. The remedies are prose that wants editing, and the check
   in section 3 already covers the mechanical surface.*
