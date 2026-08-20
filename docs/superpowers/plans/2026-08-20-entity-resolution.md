# Entity Resolution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the entity graph forking a node every time the model spells a name differently, so that a person or service discussed across several sessions is one node with one set of edges.

**Architecture:** When the graph pass declares an entity, embed its name and look for an existing entity in scope whose embedding is close enough. Above a threshold, reuse that entity; below it, create a new one and store its embedding. The ANN query is the piece that does not exist yet; the column, its HNSW index, and the write path all do.

**Tech Stack:** Go 1.25+ (toolchain in the `golang:1.26` container — no Go on this host), pgvector, pgx. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-19-entity-identity-does-not-hold.md`

## Why this exists

Two real sessions through the real hook, discussing the same person, produced two disconnected subgraphs:

```
priha-raman  -[owns]->    nightly-stock-reconciliation-job, sku-catalog, …
dana-okafor  -[covers]->  priya-raman
```

The model wrote the name one way in one session and another way in the next. Entity identity is exact string equality on a model-produced name, enforced by a unique index on `(scope, kind, name)`, so the two never met. No test caught it, because every test constructs its own entity names and is therefore consistent by construction.

The graph work on `feat/graph-extraction` is otherwise complete and measured: the write path builds sensible graphs from real text, the bridge to memory rows is fixed and proven, and the retrieval channel fires. What it has never done is surface a row vector search missed — and this is why.

## Global Constraints

- **Run the toolchain in Docker.** No `go` or `gofmt` on this host:
  `docker run --rm -v "$PWD":/src:ro -v <scratch>/gocache:/gocache -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod -w /src golang:1.26 <cmd>`
- `gofmt -s -l .` clean; `go vet ./...`; `go test -race ./...` all pass.
- **Every setting is declared in `cmd/anamnesia/settings.go` and nowhere else**, and threaded through all four stops: `settings.go`, `internal/config/config.go`, `extract.Config`, and **`cmd/anamnesia/serve.go:277`** — that last one is the only place resolved config reaches the extractor, unit tests build `extract.Config` directly and cannot catch its omission, and it has already been missed once on this branch.
- **The schema width and `embed.dims` must agree.** No migration is needed here; `entities.embedding` and its HNSW index already exist. Do not touch either.
- **Everything is behind `graph.extract`**, which defaults false. An install that leaves it off must behave exactly as it does today.
- **NEVER use `pkill`. Never stop, restart or remove any container or volume.** `anamnesia restart` with `ANAMNESIA_HOME` set to the scratch home is fine. The operator's real install is on this machine: never touch `~/.anamnesia`, port 8181, or `anamnesia-postgres`.
- No new third-party dependencies. No "Co-Authored-By" or attribution lines in commits.

## What already exists — verified before writing this

| Piece | State |
|---|---|
| `entities.embedding vector(N)` column | exists, and every dims migration rebuilds its HNSW index |
| `UpsertEntity` persisting an embedding | exists, `COALESCE(EXCLUDED.embedding, entities.embedding)` so it never clobbers |
| `EntitiesMissingEmbedding`, `SetEntityEmbedding` | exist (`internal/store/graph.go:137,161`) — **and nothing calls either** |
| An ANN query over entities | **does not exist** — this is the gap |
| The vector query pattern to copy | `internal/retrieval/retrieval.go:368-379`, `ORDER BY embedding <=> $2 ASC` with `pgvector.NewVector` |

---

### Task 1: The nearest-entity query

**Files:**
- Modify: `internal/store/graph.go`
- Test: `internal/store/graph_test.go`

**Interfaces:**
- Produces: `func (s *Store) NearestEntities(ctx context.Context, scope anamnesia.Scope, vec []float32, limit int) ([]EntityMatch, error)` and `type EntityMatch struct { Entity *anamnesia.Entity; Distance float64 }`.

Return cosine distance, not similarity, and do not invent a threshold here — the caller decides. Scope the query by `user_id` and follow the project rule the rest of the file uses: add `(project_id = $N OR project_id IS NULL)` only when the query is project-scoped. Skip rows with a NULL embedding.

- [ ] **Step 1: Write the failing test**

```go
func TestNearestEntitiesRanksByDistance(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	uid, err := st.EnsureUser(ctx, "entity-nearest-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), "entity-nearest-test") })
	scope := anamnesia.Scope{UserID: uid}

	// Three entities at known distances from the probe, using synthetic
	// vectors so the assertion does not depend on an embedding model.
	dims := 1536
	mk := func(name string, first float32) *anamnesia.Entity {
		v := make([]float32, dims)
		v[0] = first
		v[1] = 1 - first
		e := &anamnesia.Entity{Scope: scope, Kind: "person", Name: name, Embedding: v}
		if err := st.UpsertEntity(ctx, e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	near := mk("priya-raman", 1.0)
	mid := mk("priha-raman", 0.9)
	far := mk("dana-okafor", 0.0)

	probe := make([]float32, dims)
	probe[0] = 1.0
	got, err := st.NearestEntities(ctx, scope, probe, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d matches, want 3", len(got))
	}
	if got[0].Entity.ID != near.ID || got[1].Entity.ID != mid.ID || got[2].Entity.ID != far.ID {
		t.Errorf("order = %s, %s, %s; want nearest first",
			got[0].Entity.Name, got[1].Entity.Name, got[2].Entity.Name)
	}
	if !(got[0].Distance < got[1].Distance && got[1].Distance < got[2].Distance) {
		t.Errorf("distances not ascending: %v, %v, %v",
			got[0].Distance, got[1].Distance, got[2].Distance)
	}
	_ = far
}

func TestNearestEntitiesSkipsEntitiesWithoutAnEmbedding(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	uid, err := st.EnsureUser(ctx, "entity-nullvec-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), "entity-nullvec-test") })
	scope := anamnesia.Scope{UserID: uid}

	// Every entity created before this feature has a NULL embedding. They
	// must be invisible to the query rather than sorting arbitrarily.
	if err := st.UpsertEntity(ctx, &anamnesia.Entity{Scope: scope, Kind: "person", Name: "no-vector"}); err != nil {
		t.Fatal(err)
	}
	probe := make([]float32, 1536)
	probe[0] = 1
	got, err := st.NearestEntities(ctx, scope, probe, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d matches, want 0: an entity with no embedding must not match", len(got))
	}
}

func TestNearestEntitiesStaysInsideItsScope(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	mine, err := st.EnsureUser(ctx, "entity-scope-mine")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := st.EnsureUser(ctx, "entity-scope-theirs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.DeleteUser(context.Background(), "entity-scope-mine")
		_, _ = st.DeleteUser(context.Background(), "entity-scope-theirs")
	})

	v := make([]float32, 1536)
	v[0] = 1
	if err := st.UpsertEntity(ctx, &anamnesia.Entity{
		Scope: anamnesia.Scope{UserID: theirs}, Kind: "person", Name: "someone-elses", Embedding: v,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.NearestEntities(ctx, anamnesia.Scope{UserID: mine}, v, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d matches from another user's scope, want 0", len(got))
	}
}
```

Before writing these, read `anamnesia.Entity` (`pkg/anamnesia/types.go:309`) and confirm the `Embedding` field's name, and read how `UpsertEntity` (`internal/store/graph.go:22`) accepts it.

- [ ] **Step 2: Run the tests and watch them fail**

A throwaway stack is running as container `anamnesia-graph-pg` on port 5439; password in `<scratch>/graph-pg-password`.

```bash
docker run --rm --network container:anamnesia-graph-pg -v "$PWD":/src:ro -v /tmp/gocache:/gocache \
  -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod -w /src \
  -e ANAMNESIA_TEST_DATABASE_URL='postgres://anamnesia:PW@127.0.0.1:5432/anamnesia?sslmode=disable' \
  golang:1.26 go test ./internal/store/ -run TestNearestEntities -v
```

Expected: build failure — `NearestEntities` and `EntityMatch` undefined.

- [ ] **Step 3: Implement**

Follow `internal/retrieval/retrieval.go:368-379` for the query shape: `ORDER BY embedding <=> $2 ASC`, `pgvector.NewVector(vec)`, `embedding IS NOT NULL` in the WHERE. Select `embedding <=> $2` as a column so the distance can be returned. Reuse `scanEntity`'s column order rather than inventing one — read it first.

- [ ] **Step 4: Watch them pass, then commit**

```bash
git add internal/store/graph.go internal/store/graph_test.go
git commit -m "Add a nearest-entity query over the embedding that was never used"
```

---

### Task 2: Resolve before creating

**Files:**
- Modify: `internal/extract/graph.go`
- Modify: `cmd/anamnesia/settings.go`, `internal/config/config.go`, `cmd/anamnesia/serve.go`
- Test: `internal/extract/graph_test.go`, `internal/extract/graph_db_test.go`

**Interfaces:**
- Consumes: `Store.NearestEntities` (Task 1), `e.Embedder` (already on `Extractor`).
- Produces: `func (e *Extractor) resolveEntity(ctx context.Context, scope anamnesia.Scope, kind, name string) (uuid.UUID, bool, error)` — the bool reports whether an existing entity was reused.

One new setting:

```go
	{Key: "graph.merge_distance", Kind: kString, Def: "0.15", Env: "ANAMNESIA_GRAPH_MERGE_DISTANCE",
		Doc: "How close two entity names must be before they are treated as the same thing. Cosine distance, so smaller is stricter; 0 merges only identical names. Raise it if one person keeps appearing as several nodes, lower it if distinct things are being merged."},
```

`kString` rather than `kInt` because it is fractional; parse it with `strconv.ParseFloat` in `config.go` and error naming the setting if it does not parse or falls outside `[0, 1]`. **Never default silently.**

In `runGraph`, for each `ADD_ENTITY`: normalise the name as today, embed it, call `NearestEntities(scope, vec, 3)`, and if the closest match is within `graph.merge_distance` **and has the same kind**, reuse that entity's id instead of upserting a new one. Otherwise upsert as today, with the embedding attached so the next session can match against it.

Requiring the same kind is deliberate: `checkout-service` the service and `checkout-service` the project are different things, and a name-only embedding cannot tell them apart.

Record every merge on the `graph` trace step — which name was absorbed into which existing entity, and at what distance. A silent merge is indistinguishable from an extraction that happened to be consistent, and the two need telling apart when calibrating the threshold.

- [ ] **Step 1: Write the failing tests**

Unit, no database:
- a `fakeEmbedder` returning a fixed vector per string makes `resolveEntity` reuse when distance is under the threshold and create when over it
- same distance but different `kind` creates rather than merges
- an embedder error does not fail the pass: fall back to exact-name behaviour and carry on, because a graph that stops extracting is worse than one that occasionally forks

DB-backed, in `graph_db_test.go` alongside the existing one:
- **the regression this whole plan exists for**: run `runGraph` twice with two different spellings of one name — `priya-raman` then `priha-raman` — and assert ONE entity exists afterwards, with both sessions' edges attached to it. Verify it fails before the fix.

Read the existing `fakeLLM` and `graph_db_test.go` first and match their style; do not define a second fake LLM.

- [ ] **Step 2: Run them, watch them fail, implement, watch them pass**

- [ ] **Step 3: Commit**

```bash
git add internal/extract/graph.go internal/extract/graph_test.go internal/extract/graph_db_test.go \
        cmd/anamnesia/settings.go internal/config/config.go cmd/anamnesia/serve.go
git commit -m "Merge entities whose names mean the same thing"
```

---

### Task 3: Calibrate and measure

**Files:** none changed unless a defect is found.

This is the task the previous branch could not complete, and it now can.

- [ ] **Step 1: Calibrate the threshold.** Embed a list of name pairs through the real embedder and print their cosine distances, so `graph.merge_distance` is chosen from data rather than guessed. At minimum:

```
priya-raman     vs priha-raman         (must merge — the real failure)
checkout-service vs checkout-services  (should merge)
sku-catalog     vs sku-cache           (must NOT merge)
dana-okafor     vs priya-raman         (must NOT merge)
rotterdam       vs rotterdam-warehouse (judgement — report the number)
```

Report the numbers and say whether one threshold separates the merge cases from the non-merge cases. **If no threshold separates them, that is the finding** — name-only embeddings may be too weak, and the answer would be embedding more context (kind, props, a mention sentence) rather than tuning a number.

- [ ] **Step 2: Re-run the two-session proof that failed.** Same two transcripts as before — the timezone/ownership session and the sabbatical/handover session, discussing one person. Through the real hook, `graph.extract` on. Assert: one node for that person, both subgraphs connected, and then query for the first session's subject and check whether a row from the second surfaces carrying `GraphRank` **without** a vector or lexical rank.

That last assertion is the whole point of the graph, and it has never once been demonstrated end to end.

- [ ] **Step 3: Report honestly.** If the sessions still do not connect, or connect but surface nothing, say so plainly. The design commits to reconsidering rather than shipping on faith.

---

## Self-Review

**Spec coverage.** Embedding-similarity resolution on write → Tasks 1 and 2. Threshold from data not guesswork → Task 3 Step 1. The same-kind guard → Task 2. Trace visibility for merges → Task 2. The end-to-end proof → Task 3 Step 2.

**Deliberately not here:** backfilling embeddings for entities created before this. `EntitiesMissingEmbedding` and `SetEntityEmbedding` both exist and nothing calls them, so wiring the embed worker would be a small change — but it would mean existing forked nodes silently start merging on a later tick, which is a data change nobody asked for. New entities get embeddings; old ones keep exact-name behaviour until someone decides otherwise.

**The risk worth naming:** merging is not reversible. Two things wrongly judged the same become one node with both sets of edges and no record of the split. That is why the threshold is a setting, why it is calibrated from data before shipping, and why every merge is on the trace. It is also why the default should err strict: a forked node is visible and fixable, a wrongly merged one is neither.
