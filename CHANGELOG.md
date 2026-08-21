# Changelog

## Unreleased

Anamnesia became a single self-contained CLI, and the install/update paths
were rebuilt around being verifiable.

### Fixed

- **A session whose transcript was gone counted as a broken hook.**
  `SessionEnd` fires for sessions whose transcript was never written or has
  already been removed, and the checkpoint reported "no such file or
  directory" as a failure. Measured on one install: 6 of 54 session-end hooks
  failed that way, two in the same second, which looks like a batch cleanup
  of stale sessions.

  Nothing was lost, because there was nothing to ingest. But hooks record
  their outcome precisely so `doctor` can report one that silently fails
  every turn, and six false failures dilute the only signal there is. A
  missing transcript is now an outcome, not an error. A transcript that
  exists but cannot be read still fails, which is the case worth seeing.

- **An older assertion could overwrite a newer one, and did.** On a live
  install `project.schema_version` read v10 and was superseded by v8 four
  seconds later; the schema was, and is, v10. The same key took
  `project.current_release` from rc5 backwards to rc4. The extractor applies
  `UPDATE_FACT` in whatever order sources drain, and with segmentation and
  `worker.extract_concurrency` that order is not deterministic, so whichever
  assertion landed last won regardless of which described a later moment.

  A value change is now refused when the stored value was asserted from
  later content, comparing the `occurred_at` of the sources behind each.
  The refusal is a distinct error, `ErrStaleAssertion`, so a caller can tell
  "already known better" from "this did not work"; the extractor records it
  in the source's trace like any other operation outcome.

  Only extraction is constrained. A write with no source is a person stating
  what is true now, through the CLI or `anamnesia_facts_upsert`, and always
  wins. A stored row with no source never becomes unchangeable either.

- **Opening a directory created a project, before anything was stored in
  it.** `resolveScope` called `EnsureProject`, and eleven HTTP endpoints and
  seventeen MCP tools used it, most of them reads. `SessionStart` and
  `UserPromptSubmit` fire in every directory Claude Code is opened in, so
  merely starting a session filed that directory as a project. One real
  install carried 10 projects holding no sources, no facts and no
  experiences. A project is now created only by something that persists:
  ingest, a fact upsert, an experience or commitment record, a skill
  registration, a working-memory append, or a graph entity.

  An unknown project resolves to the nil uuid rather than a nil id. That
  distinction matters: `retrieval.Search` omits the project filter entirely
  when the id is nil, which means "every project", so the first prompt in a
  new repository would otherwise have read across everything the user had
  ever stored.

- **Every release after rc9 was invisible to `anamnesia update`.** The
  prerelease part of a version was compared as a plain string, so `"rc10" <
  "rc9"` because `'1' < '9'`. Tagging rc10 built and published normally, and
  then every install on rc9 reported "already on the latest release"
  forever: the release existed and simply could not be reached. `--force`
  did not help, because the up-to-date branch returns before consulting it.
  Runs of digits inside a prerelease identifier are now compared as numbers.

- **Consolidation could never run, and said it succeeded.** The clustering
  threshold was hardcoded at a cosine of 0.85, which no real corpus reaches.
  Measured over the 1,402 same-scope experience pairs on a live install: the
  mean similarity was 0.289 and the single most similar pair scored 0.754, so
  not one pair cleared the bar. No cluster of two ever formed, the LLM was
  never called, and every pass finished in about 42ms reporting
  "consolidation pass complete". The only output it had ever produced came
  from the `doctor` health check, whose rows are byte-identical and score
  1.000 — so the feature looked exercised while being inert for real memory.

  The threshold is now `worker.consolidate_similarity`, defaulting to 0.65.
  That number was chosen by replaying the same greedy clusterer over that
  corpus at each candidate value and reading what it actually merged, not by
  picking a rounder one: it forms clean topical pairs. `worker.consolidate_max_cluster`
  exposes the per-cluster cap alongside it. A threshold above 1 is now
  rejected where it is typed, because it is unreachable by any cosine and
  would reintroduce exactly this failure.

- **A single project-less experience made consolidation merge every
  project.** `activeScopes` groups by `(user_id, project_id)`, so one row
  with no project makes `(user, nil)` an active scope. The candidate query
  then omitted the project filter for that scope instead of matching
  `project_id IS NULL`, so the pass pulled in every experience the user
  owned, across every project, and clustered them together. The summary was
  written under the scope it ran as — `project_id` NULL — which the read
  path treats as user-level and returns in *every* project, because it
  matches `project_id = $n OR project_id IS NULL`. So one summary blending
  two unrelated projects leaked into all of them, and the experiences it
  covered were consolidated a second time despite already being folded
  inside their own project.

- **Every consolidation pass re-distilled clusters it had already
  distilled.** Consolidation is deliberately additive: it does not supersede
  its sources, because doing that once invalidated every source row and
  silently broke fact-grounded retrieval. But nothing replaced that guard, so
  the sources stayed eligible forever. On the same install, 8 identical rows
  had become 13 summaries across 63 source links and were still growing, one
  LLM call at a time, on every restart. A pass now skips any cluster whose
  exact membership already backs an existing summary, and reports the count it
  skipped so a pass that folds nothing still says whether it found nothing or
  had already done the work.

- **One install could take over another's database and lock it out.** The
  Postgres container's name, volume and port all live inside a config file, and
  which config file is used depends on `ANAMNESIA_HOME`. So a second install
  left on the defaults resolved to the same container as the first and simply
  adopted it. Adopting was survivable; the next step was not. The password
  reconcile exists to repair a config whose password has drifted from its own
  container, and it repairs it by rewriting the role password — against another
  install's database, that silently locks the owner out of their own memory.

  Containers now record the install that created them, in an `anamnesia.home`
  label. A container labelled for a different home is refused outright, naming
  both homes so the reader can tell which config is wrong. A container with no
  label — every container created before this — stays usable, because refusing
  those would break working installs, but its password will not be rewritten
  without `--adopt`. `--adopt` covers exactly that case and cannot override a
  label naming someone else, because a flag that seizes a container in active
  use is the same failure wearing a different hat.

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

- **Tab completion for commands, flags, and the values that go with them.**
  Cobra has generated a completion script all along and nothing ever
  installed it, so the feature existed and no user ever met it. `setup` and
  `install` now write it and add one line to `~/.zshrc` or `~/.bashrc` to
  source it; fish needs no line, because it loads its completions directory
  itself. The rc file is backed up before it is first touched,
  `--no-completion` skips the step entirely, and `uninstall` takes the line
  out again, leaving the file as it was.

  What is new beyond the stock script is that arguments complete, not only
  command and flag names. `settings.go` is already the single declaration of
  every setting, so `config get` and `config set` offer exactly the keys the
  command would accept, each with its own documentation beside it, and then
  the values an enum or a boolean setting allows. A key whose value cannot
  be enumerated, a model name or an API key, offers nothing rather than a
  guess. `project move` and `--project` complete the project slugs that
  actually exist, bounded to 300ms and silent when the database cannot be
  reached: a tab press must never hang a prompt, and a stopped stack must
  never put an error in front of one.

  The script is a shim that calls `anamnesia __complete`, so it cannot go
  stale. New commands and new settings come from whichever binary is on
  `PATH`, which is why installing it once is enough and `update` does not
  have to rewrite it. `doctor` reports it, and warns rather than fails when
  it is absent: a red doctor has to keep meaning that the install is broken.

- **Mid-session flushing, on a gated `Stop` hook.** Work no longer waits for
  a session to end: once `ingest.flush_bytes` (default 16384) of new
  transcript accumulates, or `ingest.flush_after` (default 20m) passes, the
  turn is checkpointed.

  `Stop` was retired once because it re-sent the whole transcript every turn,
  making ingest quadratic in session length. Checkpoints have been
  incremental since, so each flush sends only what is new: ten flushes across
  a session send the same bytes as one at the end, cut the same way, plus one
  trailing partial segment each — a fraction of a percent on a long session.
  The gate is what keeps it from becoming the old hook, since flushing every
  turn would cut segments at turn boundaries rather than topic boundaries.
  Setting both thresholds to 0 restores checkpointing only at PreCompact and
  SessionEnd.

- **Subagent results reach memory.** A subagent runs in a transcript of its
  own that no hook ever read, so everything it worked out was invisible: one
  session here spawned 43 of them, implementing features, reviewing branches
  and finding defects, and none of that reasoning produced a single fact. A
  `SubagentStop` hook now records what each one concluded.

  Only the final message is taken. A subagent's transcript is as long as a
  session's, so capturing all of it would mean a checkpoint per agent and a
  fan-out of forty would be forty extractions. The conclusion is the dense
  part — the verdict, the finding, the decision — and everything leading to
  it is reasoning the main session already saw summarised. Every agent is
  sent regardless of type: the surprise gate is a cheaper judge of what is
  worth keeping than a list of agent types that has to be maintained.

- **`anamnesia recover`, for transcripts no checkpoint ever claimed.** A
  checkpoint fires on PreCompact and SessionEnd; a session that crashes or is
  killed fires neither, so its last stretch of work is never ingested. It is
  not lost — Claude Code writes the transcript continuously and the offset
  file records how far anamnesia read — but nothing ever went back for it.
  Session start now spawns a sweep in the background, and it can be run by
  hand with `--dry-run` to see what it would collect.

  The only judgement it makes is deciding a session is over, and that is a
  question about the file: a transcript nobody has written to for
  `ingest.recover_idle` (default 15m) is not being written to any more. A
  recovered tail is filed under the project it came from, read from the
  working directory the transcript itself records, so it never lands under
  whichever directory the sweep happened to run in.

- **`anamnesia project prune`, for the project entries that hold nothing.**
  Projects used to be created by reading, so opening a directory filed it
  whether or not anything was ever stored there. That is fixed at the
  source, but the entries it already made remain. This lists projects with
  no rows in any table that names them and, with `--apply`, deletes those
  entries.

  It is a command rather than a worker on purpose. The only cost of an empty
  project is a line in a list, and a background process that deletes rows on
  its own to tidy a display is a poor trade against the risk. It would also
  be futile: an entry reappears the moment that directory stores something.

  Emptiness is re-checked inside the delete transaction, not trusted from
  the listing. The list a user approves is a snapshot, and a session can
  checkpoint into one of those projects in between; deleting on the strength
  of the earlier read would take real memory with it.

- **`anamnesia project move <to>`, for one product built in several
  repositories.** A project slug defaults to the repository directory name,
  so a SaaS split across an API, a core service and a docs repo becomes
  three projects that cannot see each other: the read path scopes to
  `project_id = $n OR project_id IS NULL`, and consolidation clusters
  strictly inside a scope. Run in a repository, the command moves every row
  that names its project into `<to>` and writes `.anamnesia.toml` so future
  sessions file there too.

  Both halves are needed and the order matters. Moving the rows without
  writing the file leaves the next session filing under the old name, which
  reopens the split with the older memories now elsewhere. Writing the file
  first makes the old slug unresolvable, so there is nothing left to move.

  It reports and exits unless given `--apply`, and refuses rather than
  guesses when the two projects share a fact key or skill name, naming
  each one: only one row per key can live in the target, and discarding a
  fact the user never chose to lose is not a repair. Entities in the source
  block the move outright, because merging them means repointing every edge
  at the survivor, which is its own piece of work.

- **Facts keep their history instead of being overwritten.** `UpsertFact` was
  `INSERT … ON CONFLICT DO UPDATE`, so there was one row per key forever and a
  changed value destroyed the previous one. The table has carried `valid_to`,
  `invalidated_at` and `superseded_by` since `0001` and nothing ever wrote them.
  Migration `0010` narrows the identity index to *current* rows, which is what
  lets versions accumulate while still guaranteeing exactly one live row per
  key. A changed value now inserts a new row and marks the old one superseded,
  keeping its own value, provenance and embedding; re-asserting the same value
  still updates in place, so the extractor mentioning a fact repeatedly does not
  create a version per mention. Old values stay out of your prompts unless a
  caller asks for them with `include_history` on `/v1/retrieve` — an agent shown
  "cycles to work" and "takes the tram" in one context has to work out which is
  true, so the hooks leave it off.

- **An entity graph, as a third retrieval channel.** Entities and edges are
  extracted from a whole checkpoint (opt-in, `graph.extract`), linked to the
  memory rows they came from through `entity_mentions`, and walked at query time
  to reach sources the vector and lexical channels did not. Entity resolution
  merges nodes whose names mean the same thing, judged by a batched model call
  rather than a distance threshold, so the graph stops forking a node per
  spelling across sessions. On a 30-question benchmark the graph supplied 74 of
  600 hits, 13 of which no other channel reached.

- **`worker.extract_concurrency`**, how many sources the extractor works on at
  once. Extraction is roughly 85% idle network wait, so raising this is close to
  a linear speedup at identical token cost: 9.2s per source to 0.9s at eight.
  It defaults to **1**, because it is not free of meaning — sources handled
  together stop seeing each other's facts as merge candidates, so a bulk
  backfill of related sessions can produce duplicates a serial drain would have
  merged. Raise it for benchmarks and backfills.

- **A LongMemEval harness with a retrieval-only mode.**
  `scripts/longmemeval/harness.py --mode retrieval` scores retrieval directly
  against each question's gold evidence sessions, with no answerer and no judge,
  so a run costs one search per question and carries none of two models'
  variance. Re-scoring a stored corpus takes about 25 seconds and no model
  calls. `--mode end-to-end` grades answers with LongMemEval's own judge
  prompts, ported verbatim including the separate one for abstention questions.
  The corpus definition and baseline are committed, so the numbers can be
  disagreed with by re-running them.


- **A session checkpoint is cut into segments instead of sent as one blob.** The
  extractor gets one LLM call per source with a fixed output budget — 1024
  completion tokens, capped at eight operations — so a long session had one
  call in which to cover everything it contained, and the model spent it on
  whatever the session had said most often. Novel material at the end was never
  reached. (The operation cap is not what bound it: the unsegmented run below
  produced seven operations against a cap of eight. The completion budget and
  the model's own prioritisation within a single call are what bound it.) Measured on a 32.5 KB transcript of 25
  subjects — 20 restating already-known facts, 5 genuinely novel — ingested both
  ways against the same primed store:

  ```
  whole      1 source,  7 operations — all 7 re-derive the known topics
                                       0 of 5 novel subjects survived
  segmented 25 sources, 55 operations — 5 of 5 novel subjects survived
  ```

  The hook now cuts a checkpoint where the user paused for longer than
  `ingest.segment_gap` (default 20m) or where a segment grows past
  `ingest.segment_max_bytes` (default 32768), never inside a turn, and posts
  each piece as its own source. Each one gets its own gate verdict and its own
  operation budget. Set either setting to `0` to send checkpoints whole again,
  which is exactly what earlier versions did.

  Each segment also carries the timestamp of its first turn rather than the
  moment the session closed. That matters beyond bookkeeping: decay reads
  `occurred_at`, so a morning's work used to start ageing from the moment you
  shut your laptop.

  A checkpoint that fails partway now advances its offset to the last segment
  that landed, rather than leaving it untouched. Leaving it meant the next
  checkpoint re-read the same range plus whatever had accrued since, so the
  range grew, the deadline arrived sooner, and memory silently stopped for that
  session. Re-sending a segment is cheap; skipping one is not. On `SessionEnd`
  there is no next checkpoint, so this turns "lose the whole session" into
  "lose the tail after the failure" — an improvement, not a guarantee.

  **The cost, stated plainly.** Two of them. Twenty-five sources means
  twenty-five extraction calls, twenty-five embeddings and twenty-five gate
  lookups where there was one — the work drains over worker ticks rather than
  arriving as a burst, but the total is N times larger and it reaches your model
  bill. And judging each piece independently lets near-duplicates through where
  one blob was judged once: in the measurement above, 19 of 20 restatement
  segments produced their own rows, 33 facts and 10 experiences for what is
  substantively 8 pieces of information.

  Neither is free, and the trade is still worth it — consolidation and decay
  exist to compress duplicates, and nothing recovers content that was never
  extracted. But "segmentation recovers novel content" is the earned claim;
  "segmentation is free" is not. A fact count that climbs faster than before is
  expected, not a bug.

- **`anamnesia eval` measures retrieval instead of arguing about it.** Every
  retrieval decision in the tree — RRF over score normalisation, the fusion
  constant, the candidate widths, reranking the top 4×K — rested on reasoning
  rather than a number, and a change could not be shown to have helped. The
  command ingests a committed 40-source fixture corpus through the real ingest
  path under a throwaway scope, waits for extraction and embedding to drain,
  runs 25 labelled queries, and reports recall@k, precision@k, MRR, latency and
  — the number that matters most for an agent — how many queries found nothing
  at all. The scope is deleted afterwards, and the command refuses to run if it
  already exists rather than deleting a user it did not create.

  Relevance is labelled per source rather than per row, so changing the
  extraction prompt does not invalidate the gold set. Precision divides by what
  was actually returned rather than by k, because it exists to measure noise
  injected into an agent's context, and dividing by k scores "returned three
  hits, all relevant" the same as "returned ten, three relevant".

  The report says what the corpus actually became, not just that the queue
  drained. Sources that the surprise gate skips or that fail extraction leave
  the pending queue exactly as extracted ones do, so a run could be scored
  against a smaller corpus than it ingested with nothing on screen saying so.
  The source-state breakdown and row totals are now printed, with a warning
  when any source produced nothing.

  **`--baseline`'s tolerance is measured rather than guessed.** Six
  back-to-back runs over the identical corpus gave recall@5 of 0.860 to 0.920 —
  a standard deviation of 0.021 — because extraction is a model call per source
  and every run builds a slightly different corpus from the same input. The
  tolerance is 0.05, a little over two standard deviations, and a test fails if
  the constant and the measurement ever drift apart. A regression smaller than
  that cannot be established from a single run against a single baseline; it
  needs several runs on each side. A baseline that decodes without any metrics —
  one written by an older version — is refused rather than compared as though
  every metric were a genuine zero.

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

- **Consolidation traces one scope at a time.** The activity recorder's
  detail budget is per trace, and a pass opened a single trace covering every
  scope, so on an install with several projects the budget was spent on
  whichever came first and the rest recorded only "truncated". Each scope now
  gets its own trace, labelled with its user and project, so the console can
  filter consolidation the way it filters ingest and retrieve.

- **`ingest.segment_max_bytes` now defaults to 4000, was 32768.** The cap is
  not only about topic boundaries: it bounds how much the extractor has to hold
  at once, and attention degrades over a long input, so a bigger segment does
  not mean more extracted, it means less. Measured on three real 21KB sessions
  through the production prompt, the same content yielded **14 unique facts at
  32768 and 74 at 4000** — and the ones only the smaller cap found were standing
  preferences like "branch instead of committing directly to main", which is
  exactly what memory is for. The cost is one model call per segment: those
  three sessions went from 3 calls to 18. Raise it to spend less; 0 still
  disables the size cut. Existing memory is untouched — this affects new
  checkpoints only.

- **Retrieval fails instead of returning nothing.** A configured embedder that
  errored was captured, used for a trace message, and then ignored, so `Search`
  carried on without its main channel and the caller got an ordinary empty
  result. During a credit outage `/v1/retrieve` answered `200` with no hits for
  a user holding hundreds of fully-embedded facts, which is indistinguishable
  from "you have no such memory". Same reasoning as the invariant that
  `/v1/health` must be able to fail. Having *no* embedder stays legitimate:
  that is the lexical-only local setup.

- **An unusable model completion is retried, and a truncated one gets more
  room.** Two faults wore one error. Most are transient: re-running 32 failed
  sources unchanged made 28 succeed, and `doRetry` covered 429s and 5xx but
  never a `200` carrying a body that would not parse, so one hiccup became a
  permanent hole in memory. The rest are truncation, where retrying at the same
  budget spends another call for the same result. `finish_reason`, parsed
  nowhere before, says which happened, so only `"length"` doubles the budget.
  Errors now name the cause rather than the symptom. On a benchmark corpus this
  took extraction failures from 32 to 0.

- **Provenance follows the value.** `source_id` is read as "where this content
  came from", but both the `UPDATE_FACT` path and the upsert moved it to the
  new writer on every write, including one that re-asserted the value already
  stored. A session about a play ended up owning the user's bike type. It now
  moves only when the incoming value actually differs. Corpus-wide
  misattribution fell from 35.8% to 20.5%.

- **A changed value gets a fresh embedding.** The upsert kept the old vector,
  the extractor never supplies one, and the backfill worker only looks for rows
  `WHERE embedding IS NULL` — so a fact whose value changed kept the embedding
  of its previous value permanently, findable by vector search only under
  wording it no longer had. Nothing in the system could repair it.


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
