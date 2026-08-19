# Graph Extraction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fill the `entities` and `edges` tables, which have never held a row, and let retrieval use them — so memory can answer "what relates to this" and not only "what resembles this".

**Architecture:** The hook posts one extra source per checkpoint carrying the whole text, with `kind = "claude-session-graph"`. The extractor branches on that kind and runs a graph-only LLM pass instead of the fact/experience pass. A new `entity_mentions` table links entities to the sources that mention them, which is the bridge back from a graph walk to memory rows. Retrieval gains a third candidate channel, fused by the RRF that already merges vector and lexical.

**Tech Stack:** Go 1.25+ (toolchain in the `golang:1.26` container — no Go on this host), pgx, pgvector, goose migrations, cobra. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-18-graph-extraction-design.md` — read its "Revised 2026-08-19" block first; the graph pass is per-checkpoint, not per-source, and that reverses what the original text said.

## Global Constraints

- **Run the toolchain in Docker.** No `go` or `gofmt` on this host:
  `docker run --rm -v "$PWD":/src:ro -v <scratch>/gocache:/gocache -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod -w /src golang:1.26 <cmd>`
  Drop `:ro` for `gofmt -w` or a build.
- `gofmt -s -l .` prints nothing; `go vet ./...`; `go test -race ./...` all pass.
- **Every setting is declared in `cmd/anamnesia/settings.go` and nowhere else.**
- **Never default silently.** A bad setting value is an error naming the setting.
- **Hooks never break a session**: they exit 0 whatever happens.
- **The schema width and `embed.dims` must agree.** Migration `0009` adds a table, not a vector column — do not touch embedding widths.
- **`graph.extract` defaults to false.** Every install that does not set it must behave exactly as it does today, and that is a test.
- **Retrieval must be byte-identical when the graph is empty**, which is every install today. Also a test.
- No new third-party dependencies. No "Co-Authored-By" or attribution lines in commits.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/store/migrations/0009_entity_mentions.sql` | The join table linking entities to sources. |
| `internal/store/graph.go` | `RecordMention`, `EntitiesForSources`, `SourcesForEntities`. |
| `internal/store/graph_test.go` | DB-backed tests for those three. |
| `cmd/anamnesia/settings.go` | `graph.extract`, `graph.max_ops`. |
| `cmd/anamnesia/hook.go` | Posts the extra whole-checkpoint source. |
| `internal/extract/graph.go` | The graph pass: prompt, schema, operations, executor. |
| `internal/extract/graph_test.go` | Prompt gating, name resolution, superseding — no DB where possible. |
| `internal/extract/extract.go` | Branch on source kind to run the graph pass instead of pass 1. |
| `internal/retrieval/graph.go` | The neighbour-expansion channel. |
| `internal/retrieval/retrieval.go` | Fuse the third channel. |

---

### Task 1: The `entity_mentions` bridge

**Files:**
- Create: `internal/store/migrations/0009_entity_mentions.sql`
- Modify: `internal/store/graph.go`
- Test: `internal/store/graph_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func (s *Store) RecordMention(ctx context.Context, entityID, sourceID uuid.UUID) error` — idempotent.
  - `func (s *Store) EntitiesForSources(ctx context.Context, sourceIDs []uuid.UUID) ([]*anamnesia.Entity, error)`
  - `func (s *Store) SourcesForEntities(ctx context.Context, entityIDs []uuid.UUID) ([]uuid.UUID, error)`

`edges` reference `entities(id)` at both ends and nothing else, so the graph is an island: there is no column anywhere joining a fact or an experience to an entity. Both `facts` and `experiences` carry `source_id` (migration `0003`), so sources are the bridge.

- [ ] **Step 1: Write the migration**

```sql
-- 0009_entity_mentions: the bridge between the graph and memory rows.
--
-- edges join entities to entities and nothing else, so a graph walk had no
-- way back to a fact or an experience. Both carry source_id, so an entity
-- knowing which sources mentioned it is enough to get there.
--
-- An entity is upserted once and mentioned by many sources, so a source_id
-- column on entities would be wrong; this is the honest shape.

-- +goose Up
CREATE TABLE entity_mentions (
    entity_id  UUID NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    source_id  UUID NOT NULL REFERENCES sources(id)  ON DELETE CASCADE,
    PRIMARY KEY (entity_id, source_id)
);
CREATE INDEX entity_mentions_source ON entity_mentions (source_id);

-- +goose Down
DROP TABLE IF EXISTS entity_mentions;
```

Read an existing migration first (`0007_commitments.sql`) and match its goose annotation style exactly.

Note both foreign keys cascade. `commitments` is the one table in this schema that does not, and it caused an atomic-delete failure that `anamnesia eval` still works around — do not repeat it.

- [ ] **Step 2: Write the failing tests**

```go
func TestRecordMentionIsIdempotent(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	uid, err := st.EnsureUser(ctx, "graph-mention-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), "graph-mention-test") })
	scope := anamnesia.Scope{UserID: uid}

	ent := &anamnesia.Entity{Scope: scope, Kind: "service", Name: "reconciliation-job"}
	if err := st.UpsertEntity(ctx, ent); err != nil {
		t.Fatal(err)
	}
	src := &anamnesia.Source{Scope: scope, Kind: "claude-session-graph", RawContent: "x"}
	if err := st.InsertSource(ctx, src); err != nil {
		t.Fatal(err)
	}

	// Twice: a re-extraction of the same source must not error.
	for i := 0; i < 2; i++ {
		if err := st.RecordMention(ctx, ent.ID, src.ID); err != nil {
			t.Fatalf("RecordMention call %d: %v", i+1, err)
		}
	}
	ents, err := st.EntitiesForSources(ctx, []uuid.UUID{src.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].ID != ent.ID {
		t.Errorf("EntitiesForSources = %v, want exactly the one entity", ents)
	}
}

func TestSourcesForEntitiesRoundTrips(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	uid, err := st.EnsureUser(ctx, "graph-roundtrip-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), "graph-roundtrip-test") })
	scope := anamnesia.Scope{UserID: uid}

	ent := &anamnesia.Entity{Scope: scope, Kind: "site", Name: "rotterdam"}
	if err := st.UpsertEntity(ctx, ent); err != nil {
		t.Fatal(err)
	}
	var srcIDs []uuid.UUID
	for i := 0; i < 2; i++ {
		src := &anamnesia.Source{Scope: scope, Kind: "claude-session-graph", RawContent: "x"}
		if err := st.InsertSource(ctx, src); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordMention(ctx, ent.ID, src.ID); err != nil {
			t.Fatal(err)
		}
		srcIDs = append(srcIDs, src.ID)
	}
	got, err := st.SourcesForEntities(ctx, []uuid.UUID{ent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("SourcesForEntities = %v, want both sources", got)
	}
}

func TestMentionsVanishWithTheirEntity(t *testing.T) {
	// ON DELETE CASCADE, so a purged entity cannot leave dangling rows.
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	uid, err := st.EnsureUser(ctx, "graph-cascade-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), "graph-cascade-test") })
	scope := anamnesia.Scope{UserID: uid}

	ent := &anamnesia.Entity{Scope: scope, Kind: "service", Name: "doomed"}
	if err := st.UpsertEntity(ctx, ent); err != nil {
		t.Fatal(err)
	}
	src := &anamnesia.Source{Scope: scope, Kind: "claude-session-graph", RawContent: "x"}
	if err := st.InsertSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMention(ctx, ent.ID, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM entities WHERE id = $1`, ent.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM entity_mentions WHERE entity_id = $1`, ent.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d mentions survived their entity; the cascade did not fire", n)
	}
}
```

**Verify before writing these:** `InsertSource`'s real name and signature (read `internal/store/sources.go`), and `anamnesia.Entity`'s fields (`pkg/anamnesia/types.go:309`). The brief's versions are from memory. If `InsertSource` differs, use the real one — the point is the mention table, not the source.

- [ ] **Step 3: Run the tests and watch them fail**

A throwaway stack must be running. Set one up if there is none:

```bash
export ANAMNESIA_HOME=<scratch>/anamnesia-graph-dev
./bin/anamnesia setup --no-hooks --no-start
./bin/anamnesia config set postgres.container anamnesia-graph-pg
./bin/anamnesia config set postgres.volume anamnesia-graph-pgdata
./bin/anamnesia config set postgres.port 5439
./bin/anamnesia config set server.addr 127.0.0.1:8203
./bin/anamnesia start
```

NEVER point `ANAMNESIA_HOME` at the default location — the real install lives there.

```bash
docker run --rm --network container:anamnesia-graph-pg -v "$PWD":/src:ro \
  -v /tmp/gocache:/gocache -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod \
  -e GOFLAGS=-mod=mod -w /src \
  -e ANAMNESIA_TEST_DATABASE_URL='postgres://anamnesia:<pw>@127.0.0.1:5432/anamnesia?sslmode=disable' \
  golang:1.26 go test ./internal/store/ -run 'TestRecordMention|TestSourcesForEntities|TestMentionsVanish' -v
```

Read `<pw>` from `$ANAMNESIA_HOME/config.toml`. Expected: failure — `RecordMention` undefined, and the table does not exist until the migration runs (`anamnesia migrate`, or a restart, applies it).

- [ ] **Step 4: Implement the three methods**

Append to `internal/store/graph.go`:

```go
// RecordMention notes that a source mentioned an entity. Idempotent: a
// re-extraction of the same source must not error.
func (s *Store) RecordMention(ctx context.Context, entityID, sourceID uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO entity_mentions (entity_id, source_id)
		VALUES ($1, $2)
		ON CONFLICT (entity_id, source_id) DO NOTHING`, entityID, sourceID)
	if err != nil {
		return fmt.Errorf("record mention: %w", err)
	}
	return nil
}

// EntitiesForSources returns the entities those sources mentioned. This is
// the outward half of the bridge: a search hit knows its source, and this
// turns that into somewhere to start walking.
func (s *Store) EntitiesForSources(ctx context.Context, sourceIDs []uuid.UUID) ([]*anamnesia.Entity, error) {
	if len(sourceIDs) == 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT e.id, e.user_id, e.project_id, e.kind, e.name, e.props, e.created_at
		  FROM entities e
		  JOIN entity_mentions m ON m.entity_id = e.id
		 WHERE m.source_id = ANY($1)`, sourceIDs)
	if err != nil {
		return nil, fmt.Errorf("entities for sources: %w", err)
	}
	defer rows.Close()
	return scanEntities(rows)
}

// SourcesForEntities returns the sources that mentioned those entities. The
// inward half: having walked to a neighbour, this is how its memory rows are
// reached, since facts and experiences carry source_id.
func (s *Store) SourcesForEntities(ctx context.Context, entityIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(entityIDs) == 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT source_id FROM entity_mentions WHERE entity_id = ANY($1)`, entityIDs)
	if err != nil {
		return nil, fmt.Errorf("sources for entities: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
```

`scanEntities` may not exist. Read how `Neighbors` (`graph.go:211`) scans entity rows and either reuse its helper or inline the same scan — do not invent a different column order.

- [ ] **Step 5: Run the tests and watch them pass**

Same command. Then `gofmt -s -l .`, `go vet ./...`, full `go test ./...`.

- [ ] **Step 6: Commit**

```bash
git add internal/store/migrations/0009_entity_mentions.sql internal/store/graph.go internal/store/graph_test.go
git commit -m "Bridge the graph to memory rows with entity_mentions"
```

---

### Task 2: The graph extraction pass

**Files:**
- Create: `internal/extract/graph.go`
- Create: `internal/extract/graph_test.go`
- Modify: `internal/extract/extract.go` (branch on source kind)
- Modify: `cmd/anamnesia/settings.go` (two settings)

**Interfaces:**
- Consumes: `Store.UpsertEntity`, `Store.CreateEdge`, `Store.InvalidateEdge`, `Store.RecordMention` (Task 1).
- Produces:
  - `const graphSourceKind = "claude-session-graph"`
  - `func (e *Extractor) runGraph(ctx context.Context, src *anamnesia.Source, tr *activity.Trace) (int, error)`
  - `type graphOperation struct { Op, Kind, Name, From, To string; Props map[string]any; Trust float32 }`

Settings:

```go
	// ─── graph ───────────────────────────────────────────────────────
	{Key: "graph.extract", Kind: kBool, Def: "false", Env: "ANAMNESIA_GRAPH_EXTRACT",
		Doc: "Extract entities and relationships from a session, in one extra model call per checkpoint. Off by default: it costs a call, and an install that never reads the graph should not pay for it."},
	{Key: "graph.max_ops", Kind: kInt, Def: "12", Env: "ANAMNESIA_GRAPH_MAX_OPS",
		Doc: "Caps how many entities and edges one checkpoint may produce."},
```

Wire both into `internal/config/config.go` beside `ExtractCommitments` (`config.go:74-75`, `:193`) and into `extract.Config`.

**The branch in `Run`**, placed immediately after the `MinContentLen` check and before the surprise gate:

```go
	if src.Kind == graphSourceKind {
		if !cfg.ExtractGraph {
			tr.End("skipped", "Graph extraction is off")
			return 0, nil
		}
		return e.runGraph(ctx, src, tr)
	}
```

A graph source never runs the fact pass, never runs the surprise gate, and never fetches candidates. It is a different job on the same queue.

- [ ] **Step 1: Write the failing tests**

```go
func TestGraphSourceIsIgnoredWhenTheFlagIsOff(t *testing.T) {
	ctx := context.Background()
	fake := &fakeLLM{Ops: []Operation{{Op: "NOOP"}}}
	ex := &Extractor{Cfg: Config{}, LLM: fake}
	src := &anamnesia.Source{
		Scope: anamnesia.Scope{UserID: uuid.New()}, Kind: graphSourceKind,
		OccurredAt: time.Now().UTC(),
		RawContent: "Some content long enough to clear the min-content gate.",
	}
	n, err := ex.Run(ctx, src)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 0 {
		t.Errorf("wrote %d operations with graph.extract off, want 0", n)
	}
	if fake.Calls != 0 {
		t.Errorf("made %d model calls with graph.extract off, want 0", fake.Calls)
	}
}

func TestAnOrdinarySourceNeverRunsTheGraphPass(t *testing.T) {
	// The flag being on must not change what a normal checkpoint does.
	ctx := context.Background()
	fake := &fakeLLM{Ops: []Operation{{Op: "NOOP"}}}
	ex := &Extractor{Cfg: Config{ExtractGraph: true}, LLM: fake}
	src := &anamnesia.Source{
		Scope: anamnesia.Scope{UserID: uuid.New()}, Kind: "chat-turn",
		OccurredAt: time.Now().UTC(),
		RawContent: "Some content long enough to clear the min-content gate.",
	}
	if _, err := ex.Run(ctx, src); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(fake.System, "ADD_ENTITY") {
		t.Error("an ordinary source was sent the graph prompt")
	}
}

func TestGraphPromptIsSentForAGraphSource(t *testing.T) {
	ctx := context.Background()
	fake := &fakeLLM{}
	ex := &Extractor{Cfg: Config{ExtractGraph: true}, LLM: fake}
	src := &anamnesia.Source{
		Scope: anamnesia.Scope{UserID: uuid.New()}, Kind: graphSourceKind,
		OccurredAt: time.Now().UTC(),
		RawContent: "Rotterdam writes local time; every other site writes UTC.",
	}
	if _, err := ex.Run(ctx, src); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Calls != 1 {
		t.Fatalf("made %d model calls, want exactly 1", fake.Calls)
	}
	for _, want := range []string{"ADD_ENTITY", "ADD_EDGE"} {
		if !strings.Contains(fake.System, want) {
			t.Errorf("graph prompt does not mention %s", want)
		}
	}
	if strings.Contains(fake.System, "ADD_FACT") {
		t.Error("the graph prompt offers ADD_FACT; it must not")
	}
}

func TestEdgeWithAnUnresolvableEndpointIsDropped(t *testing.T) {
	// The model names entities, not uuids. An edge naming something that
	// was never created and does not exist in scope cannot be written, and
	// dropping it silently would make the graph quietly wrong.
	ops := []graphOperation{
		{Op: "ADD_ENTITY", Kind: "site", Name: "rotterdam"},
		{Op: "ADD_EDGE", From: "rotterdam", To: "a-place-never-mentioned", Kind: "reports_to"},
	}
	resolved, dropped := resolveEdges(ops, map[string]uuid.UUID{"rotterdam": uuid.New()})
	if len(resolved) != 0 {
		t.Errorf("resolved %d edges, want 0", len(resolved))
	}
	if len(dropped) != 1 || !strings.Contains(dropped[0], "a-place-never-mentioned") {
		t.Errorf("dropped = %v, want one entry naming the missing endpoint", dropped)
	}
}

func TestEntityNamesAreNormalisedBeforeUpsert(t *testing.T) {
	// entities_identity is unique on (scope, kind, name) but dedupes on the
	// LITERAL name, so normalisation has to happen before the store call or
	// three spellings become three nodes the database is happy with.
	for _, in := range []string{"The Rotterdam Warehouse", "Rotterdam warehouse", "rotterdam  warehouse"} {
		if got := normaliseEntityName(in); got != "rotterdam-warehouse" {
			t.Errorf("normaliseEntityName(%q) = %q, want %q", in, got, "rotterdam-warehouse")
		}
	}
}
```

`fakeLLM` already exists in `internal/extract/extract_test.go` — read it and reuse it; do not define a second one. Check whether it records `Calls`; if not, add that field there rather than inventing a parallel fake.

- [ ] **Step 2: Run the tests and watch them fail**

```bash
docker run --rm -v "$PWD":/src:ro -v /tmp/gocache:/gocache \
  -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod \
  -w /src golang:1.26 go test ./internal/extract/ -run 'TestGraph|TestAnOrdinary|TestEdgeWith|TestEntityNames' -v
```

Expected: build failure — `graphSourceKind`, `ExtractGraph`, `graphOperation`, `resolveEdges`, `normaliseEntityName` all undefined.

- [ ] **Step 3: Write the graph pass**

Create `internal/extract/graph.go` with:

- `graphSourceKind = "claude-session-graph"`
- a system prompt offering exactly `ADD_ENTITY`, `ADD_EDGE` and `NOOP`, telling the model to name entities in lower case, to prefer few durable relationships over many incidental ones, and to emit `NOOP` when a session describes no relationships worth keeping. It must NOT offer ADD_FACT or ADD_EXPERIENCE.
- a JSON schema for the response, following `operationSchema`'s shape (`extract.go:648`)
- `normaliseEntityName(string) string` — lower case, collapse internal whitespace to single hyphens, trim, `slugKey` (`extract.go:514`) already does most of this — read it and reuse rather than writing a second normaliser
- `resolveEdges(ops []graphOperation, known map[string]uuid.UUID) (resolved []anamnesia.Edge, dropped []string)`
- `runGraph`, which: calls the model once, upserts entities (normalised) collecting their ids by name, resolves edges against that map and, for names not in it, against `Store.LookupEntity(ctx, scope, kind, name)` (`internal/store/graph.go:61`, verified to exist) so an edge may reference an entity a previous checkpoint created, creates the resolved edges, calls `RecordMention(entityID, src.ID)` for every entity, supersedes an existing valid edge with the same `(from, to, kind)` via `InvalidateEdge` before creating the new one, and records a `graph` trace step carrying entities upserted, edges created, edges superseded and edges dropped.

Cap the operations at `cfg.GraphMaxOps`.

- [ ] **Step 4: Run the tests and watch them pass**

Same command, then the whole suite, `gofmt -s -l .`, `go vet ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/extract/graph.go internal/extract/graph_test.go internal/extract/extract.go cmd/anamnesia/settings.go internal/config/config.go
git commit -m "Extract entities and edges from a whole checkpoint"
```

---

### Task 3: Post the graph source from the hook

**Files:**
- Modify: `cmd/anamnesia/hook.go`
- Test: `cmd/anamnesia/hook_test.go`

**Interfaces:**
- Consumes: the segment slice `doCheckpoint` already builds.
- Produces: nothing new; one extra POST per checkpoint when `graph.extract` is on.

After every segment has been posted successfully, and only then, post one more source: the whole checkpoint's text (the segments joined with "\n"), `kind = "claude-session-graph"`, `external_ref = "<session>#<offset>-graph"`, `occurred_at` = the first segment's `At`.

**It must not run when `graph.extract` is off**, and a failure to post it must NOT fail the checkpoint or hold back the offset — the segments are the memory, the graph source is an extra. Log it and move on.

- [ ] **Step 1: Write the failing tests**

```go
func TestGraphSourceIsPostedAfterTheSegments(t *testing.T) {
	hc, got := captureIngests(t)
	hc.values["graph.extract"] = "true"
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "the first subject, discussed at some length this morning"},
		[3]string{"2026-03-02T09:41:00Z", "user", "an entirely separate subject, also discussed at length"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "g1"}, "claude-session"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if len(*got) != 3 {
		t.Fatalf("posted %d sources, want 2 segments plus 1 graph source", len(*got))
	}
	last := (*got)[2]
	if last["kind"] != "claude-session-graph" {
		t.Errorf("last source kind = %v, want the graph kind", last["kind"])
	}
	content, _ := last["content"].(string)
	if !strings.Contains(content, "first subject") || !strings.Contains(content, "separate subject") {
		t.Errorf("graph source does not carry the whole checkpoint:\n%s", content)
	}
}

func TestNoGraphSourceWhenTheFlagIsOff(t *testing.T) {
	hc, got := captureIngests(t)
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "the first subject, discussed at some length this morning"},
		[3]string{"2026-03-02T09:41:00Z", "user", "an entirely separate subject, also discussed at length"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "g2"}, "claude-session"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if len(*got) != 2 {
		t.Errorf("posted %d sources with graph.extract off, want just the 2 segments", len(*got))
	}
}

func TestAFailedGraphSourceDoesNotFailTheCheckpoint(t *testing.T) {
	// The segments are the memory. The graph source is an extra, and losing
	// it must not hold back the offset and re-send everything next time.
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if posts == 3 { // the graph source, after two segments
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"source_id":"6f1c1f7a-0a1a-4a7a-9a7a-1a7a1a7a1a7a","queued":true}`))
	}))
	t.Cleanup(srv.Close)
	hc := testHostConfig(t)
	hc.values["server.url"] = srv.URL
	hc.values["graph.extract"] = "true"

	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "the first subject, discussed at some length this morning"},
		[3]string{"2026-03-02T09:41:00Z", "user", "an entirely separate subject, also discussed at length"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "g3"}, "claude-session"); err != nil {
		t.Errorf("a failed graph source failed the whole checkpoint: %v", err)
	}
	if off := readOffset("g3", path); off == 0 {
		t.Error("the offset was held back by a failed graph source; the segments all landed")
	}
}
```

- [ ] **Step 2: Run them, watch them fail, implement, watch them pass**

```bash
docker run --rm -v "$PWD":/src:ro -v /tmp/gocache:/gocache \
  -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod \
  -w /src golang:1.26 go test ./cmd/anamnesia/ -run 'TestGraphSource|TestNoGraphSource|TestAFailedGraph' -v
```

- [ ] **Step 3: Commit**

```bash
git add cmd/anamnesia/hook.go cmd/anamnesia/hook_test.go
git commit -m "Post one graph source per checkpoint"
```

---

### Task 4: The retrieval channel

**Files:**
- Create: `internal/retrieval/graph.go`
- Modify: `internal/retrieval/retrieval.go`
- Test: `internal/retrieval/graph_test.go`

**Interfaces:**
- Consumes: `Store.EntitiesForSources`, `Store.Neighbors`, `Store.SourcesForEntities` (Task 1).
- Produces: `Query` gains `GraphSeedN int` (default 5, 0 disables), `GraphFanout int` (default 10), `GraphK int` (default 20).

The walk, given the fused top `GraphSeedN` hits:

1. collect their `source_id`s
2. `EntitiesForSources` → the entities those sources mention
3. `Neighbors(entity, nil, "both", GraphFanout)`, restricted to edges valid now
4. `SourcesForEntities` on the neighbours → their sources
5. facts and experiences from those sources, in edge-trust order, as the channel's ranking

Expansion runs **after** fusion, not before: seeds are rows the existing search already believes in, so the graph adds reachable-and-related rows rather than replacing the ranking. If the graph is wrong, results degrade rather than collapse.

- [ ] **Step 1: Write the failing test that matters most**

```go
func TestAnEmptyGraphChangesNothing(t *testing.T) {
	// This is the regression that matters: the channel ships enabled on the
	// read side, and every install today has an empty graph. Results must be
	// byte-identical to no channel at all.
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	// ... build a scope with a few facts and experiences, no entities ...
	// Search twice: once with GraphSeedN 0, once with the default.
	// Assert the two hit slices are identical in id and order.
}

func TestTheGraphSurfacesARowNeitherSearchFinds(t *testing.T) {
	// The whole claim of this design, in one test: a row reachable only by
	// walking an edge, that vector and lexical search both miss.
	// ... two sources, two entities, one edge between them ...
	// ... query matching only the first source's text ...
	// ... assert the second source's row appears in the results ...
}
```

Write these out fully against the real `retrieval.Engine` API — read `retrieval.go:64` for `Search`'s signature and `Query`'s fields before filling them in.

- [ ] **Step 2: Implement, verify, commit**

```bash
git add internal/retrieval/graph.go internal/retrieval/retrieval.go internal/retrieval/graph_test.go
git commit -m "Let retrieval walk the graph"
```

---

### Task 5: Measure it

**Files:** none changed unless a defect is found.

- [ ] **Step 1:** Run `anamnesia eval` on the throwaway stack with `graph.extract` off, record recall@k, precision@k, MRR.
- [ ] **Step 2:** Turn `graph.extract` on, delete the eval scope, run again.
- [ ] **Step 3:** Report both, plus how many entities and edges the run produced.

**Expect the eval not to move much.** Its fixture corpus is 40 short sources with few cross-references, which is close to the worst case for a graph. The number that matters here is whether entities and edges are produced at all and whether the empty-graph path is genuinely inert. A recall improvement would be a bonus, not the claim.

If entities and edges come out at zero with the flag on, that is the finding — say so rather than tuning the corpus until it works.

---

## Self-Review

**Spec coverage.** `entity_mentions` bridge → Task 1. Per-checkpoint graph pass, kind-driven → Tasks 2 and 3. Settings declared once, default off → Task 2. Name normalisation before upsert → Task 2. Edge resolution by name with drops recorded → Task 2. Superseding via `InvalidateEdge` → Task 2. Trace step → Task 2. Neighbour expansion as a third RRF channel, after fusion → Task 4. Empty-graph inertness → Task 4, as the regression test. Measurement → Task 5.

**Deliberately not here:** entity embedding on write. The spec answers "yes" and it is genuinely useful, but it is a separate change — it costs an embed call per new entity and enables entity-anchored seeding that Task 4 does not use. Doing it in the same branch would confuse "the graph helps" with "entity search helps". Follow-up.

**Placeholders:** Task 4's two tests are sketched rather than written out, which is a real weakness of this plan — they depend on `retrieval.Engine`'s API, which I have not read closely enough to write against. The implementer must read `retrieval.go` and write them fully before implementing. Flagged rather than hidden.

**Type consistency:** `graphOperation`, `resolveEdges`, `normaliseEntityName`, `graphSourceKind` are defined in Task 2 and used only there. `RecordMention`/`EntitiesForSources`/`SourcesForEntities` are defined in Task 1 and consumed in Tasks 2 and 4. `ExtractGraph`/`GraphMaxOps` are added to `extract.Config` in Task 2.

**Risk worth naming:** the graph pass inherits the fixed output budget that lost novel facts before segmentation — one call over a whole session. For a long checkpoint it will capture the relationships it finds most salient and miss the rest. Acceptable for a navigational aid, but Task 5 should report entity and edge counts against transcript size so the shortfall is visible rather than assumed.
