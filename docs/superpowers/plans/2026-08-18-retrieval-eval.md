# Retrieval Eval Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `anamnesia eval`, which ingests a committed fixture corpus through the real pipeline, runs labelled queries against `/v1/retrieve`, and reports recall@k, precision@k, MRR and latency — so a retrieval or ingestion change can be shown to have helped rather than argued to have helped.

**Architecture:** A new subcommand in the existing `cmd/anamnesia` package, not a second binary. It talks to a running server over HTTP for ingest and retrieve, exactly as a real client does, and reaches the database directly only to delete its own scope afterwards. The fixture corpus is embedded in the binary with `go:embed` so the command works from any directory. Scoring is pure functions in their own file, tested without a database or a network.

**Tech Stack:** Go 1.25+ (toolchain runs in the `golang:1.26` container — there is no Go on the host), `cobra` for the command, `encoding/json`, `embed`, `pgx` via the existing `internal/store`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-18-retrieval-eval-design.md`

## Global Constraints

- **Run the toolchain in Docker.** There is no `go` or `gofmt` on this host. Use:
  `docker run --rm -v "$PWD":/src -v <scratch>/gocache:/gocache -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod -w /src golang:1.26 <cmd>`
  Mount read-only (`:ro`) when only testing; writable is needed for `gofmt -w`.
- **`make lint` must pass**: `gofmt -s -l .` empty, `go vet ./...`, `go test ./...`. CI additionally runs `go test -race ./...`.
- **Every setting is declared in `cmd/anamnesia/settings.go` and nowhere else.** This feature adds none — all knobs are flags, because an eval run is a command invocation, not a configuration.
- **Never default silently.** A bad flag value is an error naming the flag.
- **No new third-party dependencies.**
- **Not wired into CI.** The run needs a model key and a database; CI has neither. Only the pure-function and corpus-validation tests run in CI.
- **The eval scope is `anamnesia-eval` / `eval-corpus`.** The command must refuse to run if the resolved user handle is anything else.
- Do not add "Co-Authored-By" or tool-attribution lines to commits.

---

## File Structure

| File | Responsibility |
|---|---|
| `cmd/anamnesia/eval_score.go` | Pure metrics: recall@k, precision@k, MRR over a ranked list vs a relevant set. No I/O. |
| `cmd/anamnesia/eval_score_test.go` | Hand-computed metric fixtures including degenerate cases. Runs in CI. |
| `cmd/anamnesia/eval_corpus.go` | The embedded corpus and query files, their types, parsing and cross-validation. |
| `cmd/anamnesia/eval_corpus_test.go` | The shipped corpus parses and every label resolves. Runs in CI. |
| `cmd/anamnesia/testdata/eval/corpus.jsonl` | The fixture sources. Committed, reviewable in a diff. |
| `cmd/anamnesia/testdata/eval/queries.jsonl` | The labelled queries. |
| `cmd/anamnesia/eval.go` | The command: flags, the ingest → drain → retrieve → score run, report output, baseline comparison. |
| `cmd/anamnesia/eval_test.go` | Report rendering and baseline comparison, without a stack. |
| `internal/store/store.go` | Gains `DeleteUser`, used only for eval scope teardown. |
| `internal/store/store_test.go` | DB-backed test for `DeleteUser` cascade. Skipped without `ANAMNESIA_TEST_DATABASE_URL`. |

`testdata/` sits inside `cmd/anamnesia/` so `go:embed` can reach it — `embed` cannot cross package directory boundaries upward.

---

### Task 1: Scoring metrics

**Files:**
- Create: `cmd/anamnesia/eval_score.go`
- Test: `cmd/anamnesia/eval_score_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type queryScore struct { ID string; RecallAt map[int]float64; PrecisionAt map[int]float64; MRR float64; Found bool; LatencyMS int64 }`
  - `func score(id string, ranked []string, relevant []string, ks []int, latencyMS int64) queryScore`
  - `func aggregate(scores []queryScore, ks []int) aggregateScore`
  - `type aggregateScore struct { Queries int; RecallAt map[int]float64; PrecisionAt map[int]float64; MRR float64; ZeroHit int; P50MS int64; P95MS int64 }`

`ranked` is source ids in rank order, duplicates included — one source produces several rows and each can be its own hit. Deduplicate inside `score`, keeping first occurrence, because recall counts distinct sources found.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"math"
	"testing"
)

func closeTo(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestScoreCountsDistinctSourcesFound(t *testing.T) {
	// Two relevant sources; src-a is found twice (two rows from one
	// source) and must count once. src-b lands at rank 4.
	ranked := []string{"src-a", "src-x", "src-a", "src-b", "src-y"}
	s := score("q1", ranked, []string{"src-a", "src-b"}, []int{1, 5}, 120)

	closeTo(t, s.RecallAt[1], 0.5, "recall@1")   // src-a only
	closeTo(t, s.RecallAt[5], 1.0, "recall@5")   // both
	closeTo(t, s.MRR, 1.0, "MRR")                // first hit at rank 1
	if !s.Found {
		t.Error("Found = false, want true when anything relevant ranked")
	}
	if s.LatencyMS != 120 {
		t.Errorf("LatencyMS = %d, want 120", s.LatencyMS)
	}
}

func TestPrecisionCountsInjectedNoise(t *testing.T) {
	// Three of the top 5 are irrelevant. For an agent that is three
	// wasted slots of context, which is what precision measures.
	ranked := []string{"src-a", "src-x", "src-y", "src-b", "src-z"}
	s := score("q2", ranked, []string{"src-a", "src-b"}, []int{5}, 0)
	closeTo(t, s.PrecisionAt[5], 0.4, "precision@5")
}

func TestScoreWithNothingRelevantFound(t *testing.T) {
	s := score("q3", []string{"src-x", "src-y"}, []string{"src-a"}, []int{1, 5}, 0)
	closeTo(t, s.RecallAt[5], 0.0, "recall@5")
	closeTo(t, s.MRR, 0.0, "MRR")
	if s.Found {
		t.Error("Found = true, want false when no relevant source ranked")
	}
}

func TestScoreWithNoHitsAtAll(t *testing.T) {
	// A query that returned nothing must not divide by zero.
	s := score("q4", nil, []string{"src-a"}, []int{1, 5}, 0)
	closeTo(t, s.RecallAt[1], 0.0, "recall@1")
	closeTo(t, s.PrecisionAt[1], 0.0, "precision@1")
	closeTo(t, s.MRR, 0.0, "MRR")
}

func TestMRRUsesTheFirstRelevantRank(t *testing.T) {
	s := score("q5", []string{"src-x", "src-y", "src-a"}, []string{"src-a"}, []int{5}, 0)
	closeTo(t, s.MRR, 1.0/3.0, "MRR")
}

func TestAggregateReportsZeroHitQueriesAndPercentiles(t *testing.T) {
	scores := []queryScore{
		score("a", []string{"s1"}, []string{"s1"}, []int{1}, 100),
		score("b", []string{"s9"}, []string{"s1"}, []int{1}, 300),
		score("c", []string{"s1"}, []string{"s1"}, []int{1}, 200),
	}
	agg := aggregate(scores, []int{1})
	if agg.Queries != 3 {
		t.Errorf("Queries = %d, want 3", agg.Queries)
	}
	if agg.ZeroHit != 1 {
		t.Errorf("ZeroHit = %d, want 1: query b found nothing relevant", agg.ZeroHit)
	}
	closeTo(t, agg.RecallAt[1], 2.0/3.0, "mean recall@1")
	if agg.P50MS != 200 {
		t.Errorf("P50MS = %d, want 200", agg.P50MS)
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

```bash
docker run --rm -v "$PWD":/src:ro -v /tmp/gocache:/gocache \
  -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod \
  -w /src golang:1.26 go test ./cmd/anamnesia/ -run 'TestScore|TestPrecision|TestMRR|TestAggregate' -v
```

Expected: build failure — `undefined: score`, `undefined: queryScore`, `undefined: aggregate`.

- [ ] **Step 3: Write the implementation**

```go
// eval_score.go — retrieval metrics.
//
// Relevance is at source granularity: a source produces an unpredictable
// number of rows, so labelling row ids would make the gold set a hostage
// of the extraction prompt. A hit counts when its source_id is labelled.
package main

import "sort"

type queryScore struct {
	ID          string
	RecallAt    map[int]float64
	PrecisionAt map[int]float64
	MRR         float64
	Found       bool
	LatencyMS   int64
}

type aggregateScore struct {
	Queries     int
	RecallAt    map[int]float64
	PrecisionAt map[int]float64
	MRR         float64
	ZeroHit     int
	P50MS       int64
	P95MS       int64
}

// score measures one query. ranked is source ids in rank order and may
// repeat: several rows can come from one source. Recall counts distinct
// sources, so duplicates collapse to their first appearance.
func score(id string, ranked, relevant []string, ks []int, latencyMS int64) queryScore {
	want := make(map[string]bool, len(relevant))
	for _, r := range relevant {
		want[r] = true
	}
	seen := make(map[string]bool, len(ranked))
	distinct := make([]string, 0, len(ranked))
	for _, s := range ranked {
		if seen[s] {
			continue
		}
		seen[s] = true
		distinct = append(distinct, s)
	}

	s := queryScore{
		ID:          id,
		RecallAt:    make(map[int]float64, len(ks)),
		PrecisionAt: make(map[int]float64, len(ks)),
		LatencyMS:   latencyMS,
	}
	for _, k := range ks {
		hits := 0
		n := min(k, len(distinct))
		for _, src := range distinct[:n] {
			if want[src] {
				hits++
			}
		}
		if len(want) > 0 {
			s.RecallAt[k] = float64(hits) / float64(len(want))
		}
		if k > 0 {
			s.PrecisionAt[k] = float64(hits) / float64(k)
		}
	}
	for i, src := range distinct {
		if want[src] {
			s.MRR = 1.0 / float64(i+1)
			s.Found = true
			break
		}
	}
	return s
}

// aggregate means the per-query numbers and reports the two that matter
// on their own: how many queries found nothing, and how slow the slow
// ones were.
func aggregate(scores []queryScore, ks []int) aggregateScore {
	agg := aggregateScore{
		Queries:     len(scores),
		RecallAt:    make(map[int]float64, len(ks)),
		PrecisionAt: make(map[int]float64, len(ks)),
	}
	if len(scores) == 0 {
		return agg
	}
	lat := make([]int64, 0, len(scores))
	for _, s := range scores {
		for _, k := range ks {
			agg.RecallAt[k] += s.RecallAt[k]
			agg.PrecisionAt[k] += s.PrecisionAt[k]
		}
		agg.MRR += s.MRR
		if !s.Found {
			agg.ZeroHit++
		}
		lat = append(lat, s.LatencyMS)
	}
	n := float64(len(scores))
	for _, k := range ks {
		agg.RecallAt[k] /= n
		agg.PrecisionAt[k] /= n
	}
	agg.MRR /= n
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	agg.P50MS = percentile(lat, 0.50)
	agg.P95MS = percentile(lat, 0.95)
	return agg
}

// percentile takes the nearest-rank value from a sorted slice.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}
```

- [ ] **Step 4: Run the test and watch it pass**

Same command as Step 2. Expected: `ok`. Then `go vet ./...` and `gofmt -s -l .` clean.

- [ ] **Step 5: Commit**

```bash
git add cmd/anamnesia/eval_score.go cmd/anamnesia/eval_score_test.go
git commit -m "Add retrieval metrics for the eval"
```

---

### Task 2: Corpus format, embedding and validation

**Files:**
- Create: `cmd/anamnesia/eval_corpus.go`
- Create: `cmd/anamnesia/testdata/eval/corpus.jsonl` (illustrative six sources; Task 6 grows it)
- Create: `cmd/anamnesia/testdata/eval/queries.jsonl` (illustrative four queries)
- Test: `cmd/anamnesia/eval_corpus_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces:
  - `type evalSource struct { ID string; Kind string; OccurredAt time.Time; Content string }`
  - `type evalQuery struct { ID string; Text string; Relevant []string; Note string }`
  - `func loadCorpus() ([]evalSource, []evalQuery, error)` — reads the embedded files and cross-validates.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"strings"
	"testing"
)

// The gold set is the one thing in this feature that cannot be checked by
// running it: a mislabelled query silently reports a worse number forever.
func TestShippedCorpusIsValid(t *testing.T) {
	sources, queries, err := loadCorpus()
	if err != nil {
		t.Fatalf("loadCorpus: %v", err)
	}
	if len(sources) == 0 || len(queries) == 0 {
		t.Fatalf("corpus is empty: %d sources, %d queries", len(sources), len(queries))
	}

	ids := make(map[string]bool, len(sources))
	for _, s := range sources {
		if s.ID == "" || s.Content == "" {
			t.Errorf("source %+v has no id or no content", s)
		}
		if ids[s.ID] {
			t.Errorf("duplicate source id %q", s.ID)
		}
		ids[s.ID] = true
		if s.OccurredAt.IsZero() {
			t.Errorf("source %s has no occurred_at; decay reads it", s.ID)
		}
	}
	seen := make(map[string]bool, len(queries))
	for _, q := range queries {
		if seen[q.ID] {
			t.Errorf("duplicate query id %q", q.ID)
		}
		seen[q.ID] = true
		if strings.TrimSpace(q.Text) == "" {
			t.Errorf("query %s has no text", q.ID)
		}
		if len(q.Relevant) == 0 {
			t.Errorf("query %s labels nothing relevant, so it can only ever score zero", q.ID)
		}
		for _, r := range q.Relevant {
			if !ids[r] {
				t.Errorf("query %s says %q is relevant, but no such source exists", q.ID, r)
			}
		}
	}
}

func TestCorpusRejectsAnUnknownLabel(t *testing.T) {
	_, _, err := parseCorpus(
		[]byte(`{"id":"src-1","kind":"chat-turn","occurred_at":"2026-03-02T09:14:00Z","content":"hello there"}`),
		[]byte(`{"id":"q-1","text":"anything","relevant":["src-missing"]}`),
	)
	if err == nil {
		t.Fatal("a query labelling a nonexistent source was accepted")
	}
	if !strings.Contains(err.Error(), "src-missing") {
		t.Errorf("error %q does not name the offending id", err)
	}
}

func TestCorpusRejectsAnEmptyRelevantSet(t *testing.T) {
	_, _, err := parseCorpus(
		[]byte(`{"id":"src-1","kind":"chat-turn","occurred_at":"2026-03-02T09:14:00Z","content":"hello there"}`),
		[]byte(`{"id":"q-1","text":"anything","relevant":[]}`),
	)
	if err == nil {
		t.Fatal("a query with no relevant sources was accepted")
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

```bash
docker run --rm -v "$PWD":/src:ro -v /tmp/gocache:/gocache \
  -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod \
  -w /src golang:1.26 go test ./cmd/anamnesia/ -run 'TestShippedCorpusIsValid|TestCorpus' -v
```

Expected: build failure — `undefined: loadCorpus`, `undefined: parseCorpus`.

- [ ] **Step 3: Write the implementation**

```go
// eval_corpus.go — the committed fixture corpus for `anamnesia eval`.
//
// Embedded rather than read from disk so the command works from any
// directory, and committed rather than generated so a change to the gold
// set shows up in a diff and can be argued with.
package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"
)

//go:embed testdata/eval/corpus.jsonl
var corpusJSONL []byte

//go:embed testdata/eval/queries.jsonl
var queriesJSONL []byte

type evalSource struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	OccurredAt time.Time `json:"occurred_at"`
	Content    string    `json:"content"`
}

type evalQuery struct {
	ID       string   `json:"id"`
	Text     string   `json:"text"`
	Relevant []string `json:"relevant"`
	Note     string   `json:"note,omitempty"`
}

// loadCorpus parses the embedded fixture set.
func loadCorpus() ([]evalSource, []evalQuery, error) {
	return parseCorpus(corpusJSONL, queriesJSONL)
}

// parseCorpus reads both files and cross-validates them. Split out so the
// validation itself is testable against inputs that are meant to fail.
func parseCorpus(corpus, queries []byte) ([]evalSource, []evalQuery, error) {
	var sources []evalSource
	if err := eachLine(corpus, func(n int, line []byte) error {
		var s evalSource
		if err := json.Unmarshal(line, &s); err != nil {
			return fmt.Errorf("corpus line %d: %w", n, err)
		}
		if s.ID == "" {
			return fmt.Errorf("corpus line %d: no id", n)
		}
		if s.Kind == "" {
			s.Kind = "chat-turn"
		}
		sources = append(sources, s)
		return nil
	}); err != nil {
		return nil, nil, err
	}

	known := make(map[string]bool, len(sources))
	for _, s := range sources {
		if known[s.ID] {
			return nil, nil, fmt.Errorf("corpus: duplicate source id %q", s.ID)
		}
		known[s.ID] = true
	}

	var qs []evalQuery
	if err := eachLine(queries, func(n int, line []byte) error {
		var q evalQuery
		if err := json.Unmarshal(line, &q); err != nil {
			return fmt.Errorf("queries line %d: %w", n, err)
		}
		if len(q.Relevant) == 0 {
			return fmt.Errorf("queries line %d (%s): no relevant sources, so it can only score zero", n, q.ID)
		}
		for _, r := range q.Relevant {
			if !known[r] {
				return fmt.Errorf("queries line %d (%s): relevant source %q does not exist in the corpus", n, q.ID, r)
			}
		}
		qs = append(qs, q)
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return sources, qs, nil
}

// eachLine calls fn for every non-blank line, numbering from 1.
func eachLine(b []byte, fn func(n int, line []byte) error) error {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := fn(n, line); err != nil {
			return err
		}
	}
	return sc.Err()
}
```

- [ ] **Step 4: Write the illustrative fixtures**

`cmd/anamnesia/testdata/eval/corpus.jsonl` — six sources. Two of them (`src-005`, `src-006`) are **near misses**: they share vocabulary with `q-001` without answering it. A corpus without near misses measures nothing, because lexical search alone would score perfectly.

```
{"id":"src-001","kind":"chat-turn","occurred_at":"2026-03-02T09:14:00Z","content":"Spent the morning on the nightly stock reconciliation job. The discrepancies were not rounding: the Rotterdam warehouse writes timestamps in local time while every other site writes UTC, so anything moved between 00:00 and 02:00 CET was counted on the wrong day."}
{"id":"src-002","kind":"chat-turn","occurred_at":"2026-03-02T14:02:00Z","content":"Decided to normalise the warehouse timestamps at read time rather than migrating the historical rows, because the old rows are referenced by audit exports we cannot rewrite."}
{"id":"src-003","kind":"chat-turn","occurred_at":"2026-03-05T11:20:00Z","content":"Agreed the team convention: every service writes UTC and renders local time only in the presentation layer. Added a lint rule that fails a build on a naive datetime."}
{"id":"src-004","kind":"chat-turn","occurred_at":"2026-03-09T16:45:00Z","content":"The invoice PDF generator runs out of memory on batches over four thousand documents. Switched it to stream page by page instead of building the whole document in memory first."}
{"id":"src-005","kind":"chat-turn","occurred_at":"2026-03-11T10:05:00Z","content":"Rotterdam asked whether the warehouse dashboard could show stock levels per aisle. Purely a UI change, no data model impact, parked until after the quarter."}
{"id":"src-006","kind":"chat-turn","occurred_at":"2026-03-12T09:30:00Z","content":"Reviewed the nightly job schedule. Stock reconciliation runs at 02:30, invoice generation at 03:00, and the backup at 04:00. No changes needed."}
```

`cmd/anamnesia/testdata/eval/queries.jsonl` — four queries. `q-001` is the one the near misses exist for: `src-005` and `src-006` both mention Rotterdam, warehouses, stock and the nightly job, and neither explains the discrepancy.

```
{"id":"q-001","text":"why were the nightly stock counts off by a day","relevant":["src-001"],"note":"the timezone cause; src-005 and src-006 are near misses"}
{"id":"q-002","text":"how did we fix the warehouse timestamp problem","relevant":["src-002"],"note":"the decision, not the diagnosis"}
{"id":"q-003","text":"what is our convention for storing datetimes","relevant":["src-003"]}
{"id":"q-004","text":"what did we change about generating invoices","relevant":["src-004"]}
```

- [ ] **Step 5: Run the tests and watch them pass**

Same command as Step 2. Expected: `ok`. Both the shipped-corpus test and the two rejection tests pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/anamnesia/eval_corpus.go cmd/anamnesia/eval_corpus_test.go cmd/anamnesia/testdata/eval/
git commit -m "Add the eval fixture corpus and its validator"
```

---

### Task 3: Scope teardown

**Files:**
- Modify: `internal/store/store.go` (append after `LookupUserHandle`)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func (s *Store) DeleteUser(ctx context.Context, handle string) (bool, error)` — reports whether a row was deleted.

Every memory table declares `user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE`, so deleting the user row removes its facts, experiences, skills, working memory, entities, edges and sources in one statement. This exists only so an eval run cannot leave its corpus behind.

- [ ] **Step 1: Write the failing test**

```go
func TestDeleteUserCascadesItsMemory(t *testing.T) {
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

	uid, err := st.EnsureUser(ctx, "eval-cascade-test")
	if err != nil {
		t.Fatal(err)
	}
	scope := anamnesia.Scope{UserID: uid}
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, Key: "eval.cascade", Value: []byte(`{"v":1}`),
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := st.DeleteUser(ctx, "eval-cascade-test")
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if !deleted {
		t.Fatal("DeleteUser reported no row deleted")
	}
	if _, ok, err := st.LookupUser(ctx, "eval-cascade-test"); err != nil || ok {
		t.Errorf("user still resolves after delete (ok=%v, err=%v)", ok, err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM facts WHERE user_id = $1`, uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d facts survived the user delete; the cascade did not fire", n)
	}
}

func TestDeleteUserThatDoesNotExistIsNotAnError(t *testing.T) {
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
	deleted, err := st.DeleteUser(ctx, "no-such-eval-user-ever")
	if err != nil {
		t.Errorf("deleting an absent user should be a no-op, got %v", err)
	}
	if deleted {
		t.Error("reported a deletion that cannot have happened")
	}
}
```

If `UpsertFact` has a different signature in this tree, read `internal/store/facts.go` and use the real one — the point of the test is the cascade, not the fact.

- [ ] **Step 2: Run the test and watch it fail**

Start a throwaway stack first (never the real install):

```bash
export ANAMNESIA_HOME=/tmp/anamnesia-eval-dev
./bin/anamnesia setup --no-hooks --no-start
./bin/anamnesia config set postgres.container anamnesia-eval-pg
./bin/anamnesia config set postgres.volume anamnesia-eval-pgdata
./bin/anamnesia config set postgres.port 5437
./bin/anamnesia config set server.addr 127.0.0.1:8201
./bin/anamnesia start
```

Then, sharing the database container's network namespace (Postgres binds loopback, so `host.docker.internal` cannot reach it):

```bash
docker run --rm --network container:anamnesia-eval-pg -v "$PWD":/src:ro \
  -v /tmp/gocache:/gocache -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod \
  -e GOFLAGS=-mod=mod -w /src \
  -e ANAMNESIA_TEST_DATABASE_URL='postgres://anamnesia:<pw>@127.0.0.1:5432/anamnesia?sslmode=disable' \
  golang:1.26 go test ./internal/store/ -run TestDeleteUser -v
```

Read `<pw>` from `$ANAMNESIA_HOME/config.toml`. Expected: build failure — `st.DeleteUser undefined`.

- [ ] **Step 3: Write the implementation**

```go
// DeleteUser removes a user and, by cascade, every row that belongs to
// it. Only `anamnesia eval` calls this, to clean up the scope it created;
// nothing on the memory path deletes a user.
func (s *Store) DeleteUser(ctx context.Context, handle string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM users WHERE handle = $1`, handle)
	if err != nil {
		return false, fmt.Errorf("delete user %q: %w", handle, err)
	}
	return tag.RowsAffected() > 0, nil
}
```

If the column is not `handle`, read the `users` table in `internal/store/migrations/0001_init.sql` and use the real column name.

- [ ] **Step 4: Run the test and watch it pass**

Same command as Step 2. Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "Let the eval delete the scope it created"
```

---

### Task 4: The run — ingest, drain, retrieve, score

**Files:**
- Create: `cmd/anamnesia/eval.go`

**Interfaces:**
- Consumes: `loadCorpus`, `evalSource`, `evalQuery` (Task 2); `score`, `aggregate`, `queryScore`, `aggregateScore` (Task 1); `Store.DeleteUser` (Task 3); `loadHostConfig`, `hc.ServerURL()`, `hc.Token()`, `hc.DatabaseURL()` from the existing CLI.
- Produces:
  - `const evalUser = "anamnesia-eval"`, `const evalProject = "eval-corpus"`
  - `type evalReport struct { At string; Queries int; Aggregate aggregateScore; PerQuery []queryScore }`
  - `func runEvalPass(ctx context.Context, hc *hostConfig, sources []evalSource, queries []evalQuery, k int) (evalReport, error)`

- [ ] **Step 1: Write the implementation**

There is no unit test for this step: it is I/O against a live stack, and its correctness is demonstrated by the integration run in Task 5. The testable logic lives in Tasks 1 and 2 deliberately.

```go
// eval.go — `anamnesia eval`: ingest a fixture corpus through the real
// pipeline, run labelled queries, and report what retrieval returned.
//
// It talks HTTP like any other client rather than reaching into the
// store, so it measures the path a hook actually takes. The only direct
// database access is deleting the scope it created.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/flohs/anamnesia/internal/httpapi"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

const (
	evalUser    = "anamnesia-eval"
	evalProject = "eval-corpus"
	// drainTimeout bounds the wait for extraction and embedding. A run
	// scored against a half-warm index is worse than no run.
	drainTimeout = 10 * time.Minute
)

type evalReport struct {
	At        string         `json:"at"`
	Queries   int            `json:"queries"`
	Aggregate aggregateScore `json:"aggregate"`
	PerQuery  []queryScore   `json:"per_query"`
}

// runEvalPass ingests the corpus, waits for the queues to drain, runs
// every query and scores the result.
func runEvalPass(ctx context.Context, hc *hostConfig, sources []evalSource, queries []evalQuery, k int) (evalReport, error) {
	c := &evalClient{base: hc.ServerURL(), token: hc.Token(), hc: &http.Client{Timeout: 60 * time.Second}}

	// corpus id -> the source_id the server assigned, which is what hits
	// carry and therefore what scoring compares against.
	byCorpusID := make(map[string]string, len(sources))
	for _, s := range sources {
		occurred := s.OccurredAt
		var resp httpapi.IngestResponse
		err := c.post(ctx, "/v1/ingest", httpapi.IngestRequest{
			User: evalUser, Project: evalProject, Kind: s.Kind,
			ExternalRef: s.ID, OccurredAt: &occurred, Content: s.Content,
			PreserveRaw: true,
		}, &resp)
		if err != nil {
			return evalReport{}, fmt.Errorf("ingest %s: %w", s.ID, err)
		}
		byCorpusID[s.ID] = resp.SourceID.String()
	}

	if err := c.waitForDrain(ctx, drainTimeout); err != nil {
		return evalReport{}, err
	}

	ks := []int{1, 5, 10}
	scores := make([]queryScore, 0, len(queries))
	for _, q := range queries {
		var resp httpapi.RetrieveResp
		started := time.Now()
		err := c.post(ctx, "/v1/retrieve", httpapi.HookEvent{
			User: evalUser, Project: evalProject, Prompt: q.Text, K: k, OnlyRaw: true,
		}, &resp)
		if err != nil {
			return evalReport{}, fmt.Errorf("retrieve %s: %w", q.ID, err)
		}
		latency := time.Since(started).Milliseconds()

		ranked := make([]string, 0, len(resp.Hits))
		for _, h := range resp.Hits {
			if id := hitSourceID(h); id != "" {
				ranked = append(ranked, id)
			}
		}
		want := make([]string, 0, len(q.Relevant))
		for _, r := range q.Relevant {
			want = append(want, byCorpusID[r])
		}
		scores = append(scores, score(q.ID, ranked, want, ks, latency))
	}

	return evalReport{
		Queries:   len(queries),
		Aggregate: aggregate(scores, ks),
		PerQuery:  scores,
	}, nil
}

// hitSourceID reports which source a hit came from. A hit with no source
// was not extracted from the corpus — a consolidation summary, say — and
// cannot be scored against source-granularity labels.
func hitSourceID(h anamnesia.SearchHit) string {
	switch {
	case h.Fact != nil && h.Fact.SourceID != nil:
		return h.Fact.SourceID.String()
	case h.Experience != nil && h.Experience.SourceID != nil:
		return h.Experience.SourceID.String()
	}
	return ""
}

type evalClient struct {
	base  string
	token string
	hc    *http.Client
}

func (c *evalClient) post(ctx context.Context, path string, body, into any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, bytes.TrimSpace(msg))
	}
	if into == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// waitForDrain blocks until nothing is pending. Extraction is async, so
// querying before it finishes measures an index that is still filling.
func (c *evalClient) waitForDrain(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			c.base+"/v1/queue/pending?user="+evalUser+"&project="+evalProject, nil)
		if err != nil {
			return err
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		var q httpapi.QueuePendingResponse
		err = json.NewDecoder(resp.Body).Decode(&q)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if q.ExtractPending == 0 && q.EmbedPending == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("queues did not drain within %s (%d to extract, %d to embed).\n"+
				"Check `anamnesia logs`: an unconfigured or failing model leaves sources pending forever",
				timeout, q.ExtractPending, q.EmbedPending)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// deleteEvalScope removes everything the run created.
func deleteEvalScope(ctx context.Context, hc *hostConfig) error {
	st, err := store.Open(ctx, hc.DatabaseURL())
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = st.DeleteUser(ctx, evalUser)
	return err
}
```

- [ ] **Step 2: Verify it compiles**

```bash
docker run --rm -v "$PWD":/src:ro -v /tmp/gocache:/gocache \
  -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod \
  -w /src golang:1.26 go build ./... && echo "build ok"
```

Expected: `build ok`. If `store.Open` needs a logger or different arguments, read `cmd/anamnesia/serve.go:96` and match it.

- [ ] **Step 3: Commit**

```bash
git add cmd/anamnesia/eval.go
git commit -m "Add the eval run: ingest, drain, retrieve, score"
```

---

### Task 5: The command, its report, and the baseline gate

**Files:**
- Modify: `cmd/anamnesia/eval.go` (append)
- Modify: `cmd/anamnesia/main.go` (register the command)
- Test: `cmd/anamnesia/eval_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–4.
- Produces: `evalCmd`, `func renderReport(w io.Writer, r evalReport)`, `func compareToBaseline(base, now evalReport) (regressed bool, lines []string)`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"strings"
	"testing"
)

func TestReportNamesTheQueriesThatFoundNothing(t *testing.T) {
	// The aggregate can look healthy while individual queries return
	// nothing at all, and those are the sessions where memory silently
	// did not work. They have to be named, not averaged away.
	r := evalReport{
		Queries: 2,
		PerQuery: []queryScore{
			{ID: "q-001", Found: true, MRR: 1, RecallAt: map[int]float64{5: 1}, PrecisionAt: map[int]float64{5: 0.2}},
			{ID: "q-002", Found: false, RecallAt: map[int]float64{5: 0}, PrecisionAt: map[int]float64{5: 0}},
		},
		Aggregate: aggregateScore{
			Queries: 2, ZeroHit: 1, MRR: 0.5,
			RecallAt: map[int]float64{5: 0.5}, PrecisionAt: map[int]float64{5: 0.1},
		},
	}
	var sb strings.Builder
	renderReport(&sb, r)
	out := sb.String()
	if !strings.Contains(out, "q-002") {
		t.Errorf("report does not name the query that found nothing:\n%s", out)
	}
	if !strings.Contains(out, "recall@5") {
		t.Errorf("report omits recall@5:\n%s", out)
	}
}

func TestBaselineComparisonDetectsARecallRegression(t *testing.T) {
	base := evalReport{Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.80}, PrecisionAt: map[int]float64{5: 0.5}}}
	now := evalReport{Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.62}, PrecisionAt: map[int]float64{5: 0.5}}}
	regressed, lines := compareToBaseline(base, now)
	if !regressed {
		t.Error("a recall@5 drop from 0.80 to 0.62 was not reported as a regression")
	}
	if len(lines) == 0 {
		t.Error("comparison produced no explanation")
	}
}

func TestBaselineComparisonAcceptsAnImprovement(t *testing.T) {
	base := evalReport{Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.62}, PrecisionAt: map[int]float64{5: 0.5}}}
	now := evalReport{Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.71}, PrecisionAt: map[int]float64{5: 0.5}}}
	if regressed, _ := compareToBaseline(base, now); regressed {
		t.Error("an improvement was reported as a regression")
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

```bash
docker run --rm -v "$PWD":/src:ro -v /tmp/gocache:/gocache \
  -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod \
  -w /src golang:1.26 go test ./cmd/anamnesia/ -run 'TestReport|TestBaseline' -v
```

Expected: build failure — `undefined: renderReport`, `undefined: compareToBaseline`.

- [ ] **Step 3: Write the implementation**

```go
var (
	evalJSON     bool
	evalKeep     bool
	evalBaseline string
	evalK        int
)

var evalCmd = &cobra.Command{
	Use:   "eval",
	Short: "Measure retrieval against the built-in fixture corpus",
	Long: "Ingest a committed fixture corpus through the real pipeline, run\n" +
		"labelled queries against it, and report recall, precision, MRR and\n" +
		"latency.\n\n" +
		"The corpus is ingested under a dedicated user and deleted afterwards,\n" +
		"so a run cannot touch your own memory. It needs a configured model:\n" +
		"the stub extracts nothing and every query would score zero.",
	Args: cobra.NoArgs,
	RunE: runEval,
}

func init() {
	evalCmd.Flags().BoolVar(&evalJSON, "json", false, "emit the report as JSON")
	evalCmd.Flags().BoolVar(&evalKeep, "keep", false, "leave the ingested corpus in place")
	evalCmd.Flags().StringVar(&evalBaseline, "baseline", "", "compare against a previous --json report and fail on regression")
	evalCmd.Flags().IntVar(&evalK, "k", 10, "how many hits to request per query")
}

func runEval(cmd *cobra.Command, _ []string) error {
	if evalK <= 0 {
		return errors.New("--k must be a positive number")
	}
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	sources, queries, err := loadCorpus()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	// The corpus is ingested, not simulated, so a run against the wrong
	// scope would write dozens of fixture rows into real memory.
	if hc.User() == evalUser {
		return fmt.Errorf("identity.user is %q, which is the scope eval deletes afterwards", evalUser)
	}

	fmt.Fprintf(out, "Ingesting %d sources as %s/%s\n", len(sources), evalUser, evalProject)
	report, runErr := runEvalPass(ctx, hc, sources, queries, evalK)
	if !evalKeep {
		if err := deleteEvalScope(context.WithoutCancel(ctx), hc); err != nil {
			fmt.Fprintf(out, "warning: could not delete the eval scope: %v\n", err)
		}
	}
	if runErr != nil {
		return runErr
	}
	report.At = time.Now().UTC().Format(time.RFC3339)

	if evalJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		renderReport(out, report)
	}

	if evalBaseline != "" {
		raw, err := os.ReadFile(evalBaseline)
		if err != nil {
			return fmt.Errorf("read baseline: %w", err)
		}
		var base evalReport
		if err := json.Unmarshal(raw, &base); err != nil {
			return fmt.Errorf("parse baseline %s: %w", evalBaseline, err)
		}
		regressed, lines := compareToBaseline(base, report)
		fmt.Fprintln(out)
		for _, l := range lines {
			fmt.Fprintln(out, l)
		}
		if regressed {
			return errors.New("retrieval regressed against the baseline")
		}
	}
	return nil
}

// renderReport prints the aggregate, then names every query that found
// nothing. An average hides exactly the failures worth seeing.
func renderReport(w io.Writer, r evalReport) {
	a := r.Aggregate
	fmt.Fprintf(w, "\n%d queries over the fixture corpus\n\n", a.Queries)
	for _, k := range []int{1, 5, 10} {
		fmt.Fprintf(w, "  recall@%-3d %.3f      precision@%-3d %.3f\n",
			k, a.RecallAt[k], k, a.PrecisionAt[k])
	}
	fmt.Fprintf(w, "\n  MRR        %.3f\n", a.MRR)
	fmt.Fprintf(w, "  latency    p50 %d ms, p95 %d ms\n", a.P50MS, a.P95MS)
	fmt.Fprintf(w, "  found nothing: %d of %d\n", a.ZeroHit, a.Queries)
	if a.ZeroHit > 0 {
		fmt.Fprintln(w, "\n  queries that returned nothing relevant:")
		for _, q := range r.PerQuery {
			if !q.Found {
				fmt.Fprintf(w, "    %s\n", q.ID)
			}
		}
	}
}

// regressionTolerance is how far a metric may drift before it counts as a
// regression. Retrieval is not deterministic — the model, the reranker and
// the embedding service all vary run to run — so a strict comparison would
// cry wolf on every invocation.
const regressionTolerance = 0.02

// compareToBaseline reports whether the run got worse, and explains every
// metric either way.
func compareToBaseline(base, now evalReport) (bool, []string) {
	var lines []string
	regressed := false
	type metric struct {
		name       string
		base, curr float64
	}
	var metrics []metric
	for _, k := range []int{1, 5, 10} {
		metrics = append(metrics,
			metric{fmt.Sprintf("recall@%d", k), base.Aggregate.RecallAt[k], now.Aggregate.RecallAt[k]},
			metric{fmt.Sprintf("precision@%d", k), base.Aggregate.PrecisionAt[k], now.Aggregate.PrecisionAt[k]},
		)
	}
	metrics = append(metrics, metric{"MRR", base.Aggregate.MRR, now.Aggregate.MRR})
	for _, m := range metrics {
		delta := m.curr - m.base
		mark := "  "
		if delta < -regressionTolerance {
			mark = "!!"
			regressed = true
		}
		lines = append(lines, fmt.Sprintf("%s %-14s %.3f -> %.3f  (%+.3f)", mark, m.name, m.base, m.curr, delta))
	}
	return regressed, lines
}
```

Add the imports this needs: `errors`, `os`, `io`, `time`, `github.com/spf13/cobra`.

- [ ] **Step 4: Register the command**

In `cmd/anamnesia/main.go`, in the "Maintenance" group:

```go
	root.AddCommand(migrateCmd)
	root.AddCommand(evalCmd)
```

- [ ] **Step 5: Run the tests and watch them pass**

Same command as Step 2, plus the whole suite:

```bash
docker run --rm -v "$PWD":/src:ro -v /tmp/gocache:/gocache \
  -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod \
  -w /src golang:1.26 bash -c 'gofmt -s -l . && go vet ./... && go test ./...'
```

Expected: no `gofmt` output, vet clean, all packages `ok`.

- [ ] **Step 6: Run it for real against the throwaway stack**

The stack from Task 3 must have a real model configured — `stub` extracts nothing and every query scores zero, which would look like a broken eval rather than a stub:

```bash
export ANAMNESIA_HOME=/tmp/anamnesia-eval-dev
./bin/anamnesia config set openrouter.api_key <key>
./bin/anamnesia config set llm.model openai/gpt-4o-mini
./bin/anamnesia config set embed.model openai/text-embedding-3-small
./bin/anamnesia restart
./bin/anamnesia eval
./bin/anamnesia eval --json > /tmp/eval-baseline.json
./bin/anamnesia eval --baseline /tmp/eval-baseline.json
```

Expect: a report naming any zero-hit queries; a second run comparing clean against the first. Then confirm the scope was deleted:

```bash
curl -s 'localhost:8201/v1/users' | grep -c anamnesia-eval   # expect 0
```

- [ ] **Step 7: Commit**

```bash
git add cmd/anamnesia/eval.go cmd/anamnesia/eval_test.go cmd/anamnesia/main.go
git commit -m "Add anamnesia eval"
```

---

### Task 6: Grow the corpus to a useful size

**Files:**
- Modify: `cmd/anamnesia/testdata/eval/corpus.jsonl`
- Modify: `cmd/anamnesia/testdata/eval/queries.jsonl`

**Interfaces:**
- Consumes: the validator from Task 2, which is the acceptance gate.
- Produces: nothing new in code.

Six sources and four queries prove the machinery. They do not measure retrieval: with four queries, one changing rank moves recall by 0.25, which is noise, not signal. Grow to roughly 40 sources and 25 queries.

Authoring rules, all enforceable by reading the diff:

1. **Every query needs at least one near miss** — a source sharing its vocabulary that does not answer it. Without them lexical search alone scores perfectly and the eval cannot distinguish a good ranker from a bad one.
2. **Vary the answer shape.** Some queries answered by a fact ("what is our datetime convention"), some by an experience ("why were the counts off"), some by both.
3. **Include multi-source queries** — at least five queries whose `relevant` set has two or three entries, since recall over a single-item set is only ever 0 or 1.
4. **Include time-separated near-duplicates** — the same subject revisited months apart with a different conclusion, so decay and recency have something to sort.
5. **Keep sources in the 200–1500 character range.** Shorter than 200 risks the extractor's `MinContentLen`; much longer and one source covers several queries, blurring the labels.
6. **No secrets, no real customer names, nothing unpublishable.** This file is committed.

- [ ] **Step 1: Write the corpus**

Append sources to `corpus.jsonl` with sequential ids continuing from `src-006`, and queries to `queries.jsonl` continuing from `q-004`, following the six rules above.

- [ ] **Step 2: Run the validator**

```bash
docker run --rm -v "$PWD":/src:ro -v /tmp/gocache:/gocache \
  -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod \
  -w /src golang:1.26 go test ./cmd/anamnesia/ -run 'TestShippedCorpusIsValid|TestCorpus' -v
```

Expected: PASS. A failure names the offending id.

- [ ] **Step 3: Establish the baseline**

```bash
./bin/anamnesia eval --json > docs/superpowers/plans/eval-baseline-rc7.json
./bin/anamnesia eval        # read it, sanity-check the zero-hit list
```

If more than about a fifth of queries find nothing, the corpus is probably mislabelled or the sources are too long — read the zero-hit queries before accepting the number as a baseline.

- [ ] **Step 4: Commit**

```bash
git add cmd/anamnesia/testdata/eval/ docs/superpowers/plans/eval-baseline-rc7.json
git commit -m "Grow the eval corpus to a measurable size, and record the rc7 baseline"
```

---

## Self-Review

**Spec coverage.** Corpus format and source-granularity relevance → Task 2. Near misses → Tasks 2 and 6. recall@k, MRR, latency → Task 1. Precision, added to the spec after review → Task 1. Zero-hit count → Tasks 1 and 5. `anamnesia eval` as a subcommand, `--json`, `--baseline`, `--keep` → Task 5. Dedicated scope, refusal to run against the default, deletion afterwards → Tasks 3, 4 and 5. Drain-before-scoring → Task 4. Scorer tests and gold-set validation in CI → Tasks 1 and 2. Not wired into CI → honoured; no workflow file is touched.

**Deviation from the spec, deliberate:** the spec says roughly 60 sources and 40 queries. This plan ships 6 and 4 in Task 2 to get the machinery under test, then grows to roughly 40 and 25 in Task 6. Sizing was not load-bearing in the spec; getting the validator running before authoring the content is.

**Placeholders:** none. Every step carries the code it needs. Two steps say "if the signature differs, read the file and match it" — that is instruction, not a placeholder, and it names the file.

**Type consistency:** `queryScore` and `aggregateScore` are defined in Task 1 and used unchanged in Tasks 4 and 5. `evalSource` and `evalQuery` are defined in Task 2 and consumed in Task 4. `evalReport` is defined in Task 4 and consumed in Task 5. `DeleteUser` returns `(bool, error)` in Task 3 and is called for its error only in Task 4, which is fine. `renderReport` takes `io.Writer` in both its test and its implementation.

**Corrected at pre-flight:** an earlier draft of Task 5 carried a deliberately broken test stub as a teaching device. A plan must not mandate what the review rubric treats as a defect, and an implementer transcribing it verbatim would have shipped a function that compiles and tests nothing. Removed.
