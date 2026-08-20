package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/embed"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// marshalGraphOps builds the raw envelope entries a fakeLLM.RawOps needs
// from graphOperation values — graphOperation isn't the Operation type
// fakeLLM.Ops marshals, since it carries name/from/to/props fields
// Operation has no room for.
func marshalGraphOps(t *testing.T, ops []graphOperation) []json.RawMessage {
	t.Helper()
	raw := make([]json.RawMessage, len(ops))
	for i, op := range ops {
		b, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		raw[i] = b
	}
	return raw
}

// TestRunGraphAgainstRealStore exercises the store-touching body of
// runGraph — entity upsert, edge resolution, edge creation, superseding
// and mention recording — none of which any DB-free test in graph_test.go
// reaches, since they all stop before the store (flag off, wrong source
// kind, or an empty/NOOP op list). Reads ANAMNESIA_TEST_DATABASE_URL; if
// absent it skips so the unit test suite stays green offline.
func TestRunGraphAgainstRealStore(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	user := "graph-extract-test-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, user)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), user) })
	scope := anamnesia.Scope{UserID: uid}

	newSource := func(content string) *anamnesia.Source {
		src := &anamnesia.Source{
			Scope:      scope,
			Kind:       graphSourceKind,
			OccurredAt: time.Now().UTC(),
			RawContent: content,
		}
		if err := st.InsertSource(ctx, src); err != nil {
			t.Fatalf("insert source: %v", err)
		}
		return src
	}
	run := func(src *anamnesia.Source, ops []graphOperation) int {
		ex := &Extractor{Cfg: Config{ExtractGraph: true}, Store: st, LLM: &fakeLLM{RawOps: marshalGraphOps(t, ops)}}
		n, err := ex.Run(ctx, src)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return n
	}

	// First checkpoint: two entities and the edge between them.
	src1 := newSource("the stock-reconciliation service reads from the Rotterdam warehouse nightly.")
	n := run(src1, []graphOperation{
		{Op: "ADD_ENTITY", Kind: "site", Name: "The Rotterdam Warehouse"},
		{Op: "ADD_ENTITY", Kind: "service", Name: "stock-reconciliation"},
		{Op: "ADD_EDGE", From: "stock-reconciliation", To: "The Rotterdam Warehouse", Kind: "reads_from", Trust: 0.8},
	})
	if n != 3 {
		t.Fatalf("first checkpoint executed = %d, want 3 (2 entities + 1 edge)", n)
	}

	warehouse, err := st.LookupEntity(ctx, scope, "site", "rotterdam-warehouse")
	if err != nil {
		t.Fatalf("lookup warehouse: %v", err)
	}
	service, err := st.LookupEntity(ctx, scope, "service", "stock-reconciliation")
	if err != nil {
		t.Fatalf("lookup service: %v", err)
	}

	_, edges, err := st.Neighbors(ctx, service.ID, []string{"reads_from"}, "out", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].To != warehouse.ID {
		t.Fatalf("expected exactly one reads_from edge to the warehouse, got %v", edges)
	}

	mentioned, err := st.EntitiesForSources(ctx, []uuid.UUID{src1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(mentioned) != 2 {
		t.Errorf("EntitiesForSources(src1) = %d, want 2 (one mention per entity)", len(mentioned))
	}

	// Second checkpoint: the same graph, re-declared. Must not duplicate
	// the entities, and must supersede the existing edge rather than
	// create a second live one.
	src2 := newSource("same as before — reconciliation still reads from the Rotterdam warehouse.")
	run(src2, []graphOperation{
		{Op: "ADD_ENTITY", Kind: "site", Name: "The Rotterdam Warehouse"},
		{Op: "ADD_ENTITY", Kind: "service", Name: "stock-reconciliation"},
		{Op: "ADD_EDGE", From: "stock-reconciliation", To: "The Rotterdam Warehouse", Kind: "reads_from", Trust: 0.9},
	})

	all, err := st.ListEntities(ctx, scope, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("after a repeat checkpoint, ListEntities = %d, want still 2 (no duplicate nodes)", len(all))
	}
	_, edges, err = st.Neighbors(ctx, service.ID, []string{"reads_from"}, "out", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Errorf("after a repeat checkpoint, %d live reads_from edges exist, want 1 (superseded, not duplicated)", len(edges))
	}

	// Third checkpoint: only an edge, naming both entities by name with
	// NO ADD_ENTITY in this pass. This is the case LookupEntitiesByName
	// exists for — resolving against an entity a previous checkpoint
	// created, not one this pass re-declared.
	src3 := newSource("reconciliation now also reports its results to the Rotterdam warehouse team.")
	n = run(src3, []graphOperation{
		{Op: "ADD_EDGE", From: "stock-reconciliation", To: "The Rotterdam Warehouse", Kind: "reports_to", Trust: 0.6},
	})
	if n != 1 {
		t.Errorf("third checkpoint executed = %d, want 1 (the cross-checkpoint edge)", n)
	}
	_, edges, err = st.Neighbors(ctx, service.ID, []string{"reports_to"}, "out", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].To != warehouse.ID {
		t.Errorf("cross-checkpoint edge did not resolve without re-declaring its entities: %v", edges)
	}

	// An ambiguous name — two entities share it under different kinds —
	// must drop the edge rather than guess which one was meant.
	if err := st.UpsertEntity(ctx, &anamnesia.Entity{Scope: scope, Kind: "service", Name: "signal"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEntity(ctx, &anamnesia.Entity{Scope: scope, Kind: "channel", Name: "signal"}); err != nil {
		t.Fatal(err)
	}
	src4 := newSource("reconciliation notifies signal whenever stock drifts out of tolerance.")
	n = run(src4, []graphOperation{
		{Op: "ADD_EDGE", From: "stock-reconciliation", To: "signal", Kind: "notifies"},
	})
	if n != 0 {
		t.Errorf("fourth checkpoint executed = %d for an ambiguous edge endpoint, want 0 (dropped, not guessed)", n)
	}
	_, edges, err = st.Neighbors(ctx, service.ID, []string{"notifies"}, "out", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Errorf("an ambiguous endpoint produced an edge anyway: %v", edges)
	}
}

// TestGraphBridgeConnectsSegmentSourcesToEntities proves the property that
// was false before the fix in
// docs/superpowers/specs/2026-08-19-the-graph-bridge-is-broken.md: given a
// checkpoint whose segments produced facts, after runGraph executes,
// EntitiesForSources on those SEGMENT source ids — not the graph source's
// own id — returns the entities the graph pass found. That is exactly the
// join graphExpand relies on to seed a walk from a real search hit, which
// always carries a segment source id, never the graph source's.
//
// The graph source's metadata is round-tripped through the store via
// GetSource before runGraph sees it, the way the extractor's worker loop
// actually loads a pending source — so segment_source_ids arrives as
// []any of strings, not the []string a test could set up and forget to
// convert.
func TestGraphBridgeConnectsSegmentSourcesToEntities(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	user := "graph-bridge-test-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, user)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), user) })
	scope := anamnesia.Scope{UserID: uid}

	// Two segments, the way a real checkpoint posts one source per topic.
	// Each produces a fact — standing in for the memory a real search hit
	// returns, carrying that segment's source_id.
	newSegment := func(content string) *anamnesia.Source {
		seg := &anamnesia.Source{Scope: scope, Kind: "claude-session", OccurredAt: time.Now().UTC(), RawContent: content}
		if err := st.InsertSource(ctx, seg); err != nil {
			t.Fatalf("insert segment: %v", err)
		}
		return seg
	}
	seg1 := newSegment("I just switched the stock-reconciliation service to read from the Rotterdam warehouse nightly.")
	seg2 := newSegment("I just confirmed reconciliation now reports its results to the Rotterdam warehouse team.")

	factExtractor := &Extractor{Store: st, LLM: &fakeLLM{Ops: []Operation{
		{Op: "ADD_FACT", Key: "reconciliation.source", Value: json.RawMessage(`"rotterdam warehouse"`)},
	}}}
	for _, seg := range []*anamnesia.Source{seg1, seg2} {
		n, err := factExtractor.Run(ctx, seg)
		if err != nil {
			t.Fatalf("run segment %s: %v", seg.ID, err)
		}
		if n == 0 {
			t.Fatalf("segment %s produced no facts; test setup is invalid", seg.ID)
		}
	}

	// The graph source, carrying the segment ids the hook fix collects —
	// mirroring what doCheckpoint posts once per checkpoint, after every
	// segment has landed.
	graphSrc := &anamnesia.Source{
		Scope: scope, Kind: graphSourceKind, OccurredAt: time.Now().UTC(),
		RawContent: seg1.RawContent + "\n" + seg2.RawContent,
		Metadata: map[string]any{
			"segment_source_ids": []string{seg1.ID.String(), seg2.ID.String()},
		},
	}
	if err := st.InsertSource(ctx, graphSrc); err != nil {
		t.Fatalf("insert graph source: %v", err)
	}
	loaded, err := st.GetSource(ctx, graphSrc.ID)
	if err != nil {
		t.Fatalf("reload graph source: %v", err)
	}

	graphExtractor := &Extractor{Cfg: Config{ExtractGraph: true}, Store: st, LLM: &fakeLLM{RawOps: marshalGraphOps(t, []graphOperation{
		{Op: "ADD_ENTITY", Kind: "service", Name: "stock-reconciliation"},
		{Op: "ADD_ENTITY", Kind: "site", Name: "Rotterdam warehouse"},
		{Op: "ADD_EDGE", From: "stock-reconciliation", To: "Rotterdam warehouse", Kind: "reads_from", Trust: 0.8},
	})}}
	if _, err := graphExtractor.Run(ctx, loaded); err != nil {
		t.Fatalf("run graph pass: %v", err)
	}

	mentioned, err := st.EntitiesForSources(ctx, []uuid.UUID{seg1.ID, seg2.ID})
	if err != nil {
		t.Fatalf("entities for sources: %v", err)
	}
	if len(mentioned) != 2 {
		t.Fatalf("EntitiesForSources(segment ids) = %d entities, want 2 — this is the exact join graphExpand relies on to seed a walk from a search hit", len(mentioned))
	}
}

// vec1536 pads a 3-component test vector out to the schema's real
// embedding width (1536 for the dims this test database migrated to).
// The extra zero components do not change any cosine distance.
func vec1536(x, y, z float32) []float32 {
	v := make([]float32, 1536)
	v[0], v[1], v[2] = x, y, z
	return v
}

// newGraphTestStore opens the test database and a fresh user scope,
// registering cleanup, so each identity-resolution test below doesn't
// repeat the same boilerplate.
func newGraphTestStore(t *testing.T, userPrefix string) (*store.Store, anamnesia.Scope) {
	t.Helper()
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	user := userPrefix + "-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, user)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), user) })
	return st, anamnesia.Scope{UserID: uid}
}

// TestEntityResolutionMergesTwoSpellingsOfOneName is the regression this
// whole plan exists for:
// docs/superpowers/specs/2026-08-19-entity-identity-does-not-hold.md
// found two real sessions discussing one person land as two
// disconnected subgraphs — "priha-raman" in one session, "priya-raman"
// in the next — because entity identity was exact string equality on a
// model-produced name. Session 2's "priha-raman" recalls priya-raman as
// a candidate (fakeEmbedder places them close), and the disambiguation
// call is scripted to affirm the match — standing in for what a real
// model reading "Priha Raman... while she is on leave" right after
// creating "Priya Raman" would very plausibly conclude, though this
// test cannot prove a real model WOULD (fakeLLM is scripted, not
// reasoning) — that is what Task 3's real end-to-end proof is for. What
// this test does prove: when the model affirms, the plumbing correctly
// lands both sessions on one entity with both sets of edges attached.
func TestEntityResolutionMergesTwoSpellingsOfOneName(t *testing.T) {
	st, scope := newGraphTestStore(t, "entity-resolution-test")
	ctx := context.Background()

	emb := &fakeEmbedder{Vecs: map[string][]float32{
		"priya-raman":                      vec1536(1, 0, 0),
		"nightly-stock-reconciliation-job": vec1536(0, 0, 1),
		"dana-okafor":                      vec1536(0, 1, 0),
		"priha-raman":                      vec1536(0.99, 0.01, 0),
	}}
	newSource := func(content string) *anamnesia.Source {
		src := &anamnesia.Source{Scope: scope, Kind: graphSourceKind, OccurredAt: time.Now().UTC(), RawContent: content}
		if err := st.InsertSource(ctx, src); err != nil {
			t.Fatalf("insert source: %v", err)
		}
		return src
	}

	src1 := newSource("Priya Raman owns the nightly stock reconciliation job.")
	fake1 := &fakeLLM{RawOps: marshalGraphOps(t, []graphOperation{
		{Op: "ADD_ENTITY", Kind: "person", Name: "priya-raman"},
		{Op: "ADD_ENTITY", Kind: "project", Name: "nightly-stock-reconciliation-job"},
		{Op: "ADD_EDGE", From: "priya-raman", To: "nightly-stock-reconciliation-job", Kind: "owns"},
	})}
	ex1 := &Extractor{Cfg: Config{ExtractGraph: true, GraphCandidateDistance: 0.45}, Store: st, Embedder: emb, LLM: fake1}
	if _, err := ex1.Run(ctx, src1); err != nil {
		t.Fatalf("run session 1: %v", err)
	}
	if fake1.Calls != 1 {
		t.Errorf("session 1 made %d model calls, want 1: no entities exist yet, so nothing has a candidate and the disambiguation call must not run", fake1.Calls)
	}

	raman, err := st.LookupEntity(ctx, scope, "person", "priya-raman")
	if err != nil {
		t.Fatalf("lookup priya-raman: %v", err)
	}

	// Session 2: the disambiguation call is scripted to affirm priha-raman
	// is priya-raman — the judgment a real model would need to make from
	// context, which this test cannot exercise, only the plumbing that
	// follows from it.
	src2 := newSource("Dana Okafor covers priha raman while she is on leave.")
	fake2 := &fakeLLM{
		RawOps: marshalGraphOps(t, []graphOperation{
			{Op: "ADD_ENTITY", Kind: "person", Name: "dana-okafor"},
			{Op: "ADD_ENTITY", Kind: "person", Name: "priha-raman"},
			{Op: "ADD_EDGE", From: "dana-okafor", To: "priha-raman", Kind: "covers"},
		}),
		Verdicts: []identityVerdict{{Entity: "priha-raman", SameAs: raman.ID.String()}},
	}
	ex2 := &Extractor{Cfg: Config{ExtractGraph: true, GraphCandidateDistance: 0.45}, Store: st, Embedder: emb, LLM: fake2}
	if _, err := ex2.Run(ctx, src2); err != nil {
		t.Fatalf("run session 2: %v", err)
	}
	if fake2.Calls != 2 {
		t.Errorf("session 2 made %d model calls, want 2: priha-raman has a candidate (priya-raman), so the disambiguation call must run", fake2.Calls)
	}

	people, err := st.ListEntities(ctx, scope, "person", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 2 {
		names := make([]string, len(people))
		for i, p := range people {
			names[i] = p.Name
		}
		t.Fatalf("ListEntities(person) = %d, want 2 (the merged priya/priha-raman, and dana-okafor); got %v", len(people), names)
	}

	_, ownsEdges, err := st.Neighbors(ctx, raman.ID, []string{"owns"}, "out", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ownsEdges) != 1 {
		t.Errorf("owns edges from the merged entity = %d, want 1 (from the first session)", len(ownsEdges))
	}

	_, coversEdges, err := st.Neighbors(ctx, raman.ID, []string{"covers"}, "in", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(coversEdges) != 1 {
		t.Errorf("covers edges into the merged entity = %d, want 1 (from the second session)", len(coversEdges))
	}
}

// TestEntityResolutionKeepsSimilarButDifferentPeopleApart is the case
// distance-threshold merging could never pass: measured against the
// real embedder, priya-raman/priya-ramanujan (0.1165) sits INSIDE any
// distance bound wide enough to also catch the priya/priha-raman typo
// (0.2349) — so only a judgment, not a number, keeps them apart. The
// fakeEmbedder here places them just as close, priya-ramanujan recalls
// priya-raman as a candidate (proving the disambiguation call must
// run), and the model is scripted to NOT affirm a match — standing in
// for the judgment a real model makes reading "Ramanujan" is not a
// misspelling of "Raman". Without this test, nothing catches a
// regression back to distance-only merging.
func TestEntityResolutionKeepsSimilarButDifferentPeopleApart(t *testing.T) {
	st, scope := newGraphTestStore(t, "entity-resolution-distinct-test")
	ctx := context.Background()

	emb := &fakeEmbedder{Vecs: map[string][]float32{
		"priya-raman":     vec1536(1, 0, 0),
		"priya-ramanujan": vec1536(0.95, 0.05, 0), // ~0.001 distance: well inside any workable candidate bound
	}}
	newSource := func(content string) *anamnesia.Source {
		src := &anamnesia.Source{Scope: scope, Kind: graphSourceKind, OccurredAt: time.Now().UTC(), RawContent: content}
		if err := st.InsertSource(ctx, src); err != nil {
			t.Fatalf("insert source: %v", err)
		}
		return src
	}

	src1 := newSource("Priya Raman owns the nightly stock reconciliation job.")
	fake1 := &fakeLLM{RawOps: marshalGraphOps(t, []graphOperation{{Op: "ADD_ENTITY", Kind: "person", Name: "priya-raman"}})}
	ex1 := &Extractor{Cfg: Config{ExtractGraph: true, GraphCandidateDistance: 0.45}, Store: st, Embedder: emb, LLM: fake1}
	if _, err := ex1.Run(ctx, src1); err != nil {
		t.Fatalf("run session 1: %v", err)
	}

	// Session 2: priya-ramanujan recalls priya-raman as a candidate, but
	// the model — scripted here with no affirming verdict — judges them
	// different people, the way a real model reading "Ramanujan joined
	// the platform team" (nothing to do with Raman) plausibly would.
	src2 := newSource("Priya Ramanujan joined the platform team this week.")
	fake2 := &fakeLLM{RawOps: marshalGraphOps(t, []graphOperation{{Op: "ADD_ENTITY", Kind: "person", Name: "priya-ramanujan"}})}
	ex2 := &Extractor{Cfg: Config{ExtractGraph: true, GraphCandidateDistance: 0.45}, Store: st, Embedder: emb, LLM: fake2}
	if _, err := ex2.Run(ctx, src2); err != nil {
		t.Fatalf("run session 2: %v", err)
	}
	if fake2.Calls != 2 {
		t.Fatalf("session 2 made %d model calls, want 2: priya-ramanujan has a candidate (priya-raman) within any workable bound, so the disambiguation call must run — this test proves nothing if it didn't", fake2.Calls)
	}

	people, err := st.ListEntities(ctx, scope, "person", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 2 {
		names := make([]string, len(people))
		for i, p := range people {
			names[i] = p.Name
		}
		t.Fatalf("ListEntities(person) = %d, want 2 (priya-raman and priya-ramanujan must stay separate); got %v", len(people), names)
	}
}

// TestIdentityCallFailureFallsBackToCreatingSeparately: an unavailable
// judge must never merge, only fail open to creating separately — a
// guessed merge is exactly the failure this whole design exists to
// avoid. priha-raman has a real candidate (priya-raman), but the
// disambiguation call errors, so it must still be created as its own
// entity rather than dropped or silently merged.
func TestIdentityCallFailureFallsBackToCreatingSeparately(t *testing.T) {
	st, scope := newGraphTestStore(t, "entity-resolution-judge-down-test")
	ctx := context.Background()

	emb := &fakeEmbedder{Vecs: map[string][]float32{
		"priya-raman": vec1536(1, 0, 0),
		"priha-raman": vec1536(0.99, 0.01, 0),
	}}
	newSource := func(content string) *anamnesia.Source {
		src := &anamnesia.Source{Scope: scope, Kind: graphSourceKind, OccurredAt: time.Now().UTC(), RawContent: content}
		if err := st.InsertSource(ctx, src); err != nil {
			t.Fatalf("insert source: %v", err)
		}
		return src
	}

	src1 := newSource("Priya Raman owns the nightly stock reconciliation job.")
	fake1 := &fakeLLM{RawOps: marshalGraphOps(t, []graphOperation{{Op: "ADD_ENTITY", Kind: "person", Name: "priya-raman"}})}
	ex1 := &Extractor{Cfg: Config{ExtractGraph: true, GraphCandidateDistance: 0.45}, Store: st, Embedder: emb, LLM: fake1}
	if _, err := ex1.Run(ctx, src1); err != nil {
		t.Fatalf("run session 1: %v", err)
	}

	src2 := newSource("Priha Raman is back from leave.")
	fake2 := &fakeLLM{
		RawOps:      marshalGraphOps(t, []graphOperation{{Op: "ADD_ENTITY", Kind: "person", Name: "priha-raman"}}),
		VerdictsErr: errors.New("the judge model is unavailable"),
	}
	ex2 := &Extractor{Cfg: Config{ExtractGraph: true, GraphCandidateDistance: 0.45}, Store: st, Embedder: emb, LLM: fake2}
	n, err := ex2.Run(ctx, src2)
	if err != nil {
		t.Fatalf("run must not fail even though the disambiguation call errored: %v", err)
	}
	if n == 0 {
		t.Error("executed = 0; priha-raman should still have been created, not dropped")
	}

	people, err := st.ListEntities(ctx, scope, "person", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 2 {
		names := make([]string, len(people))
		for i, p := range people {
			names[i] = p.Name
		}
		t.Fatalf("ListEntities(person) = %d, want 2 (a judge that's down must never merge); got %v", len(people), names)
	}
}

// TestEntityCandidatesForNameFindsARealVariant guards against exactly
// the failure the graph-bridge incident already taught this project
// once (docs/superpowers/specs/2026-08-19-the-graph-bridge-is-broken.md
// — a hand-built fixture proved a mechanism production could never
// assemble): every other test in this file injects candidates via
// fakeEmbedder, so none of them can observe an empty candidate list. An
// earlier version of this design embedded the whole checkpoint's
// CONTENT and matched it against entity NAME vectors — content-to-name
// is a different regime than what graph.candidate_distance was
// calibrated against, and measured against the real embedder it never
// retrieved anything (content/priha-raman = 0.7614, bound 0.45:
// MISSED). This test would have caught that immediately.
//
// It calls a REAL configured embedding provider — skipped, like the
// database tests, when nothing is configured to run it against — and
// asserts entityCandidatesForName (not just the recall function
// underneath it — the real Extractor method, so Cfg.applyDefaults()
// and the Store binding are exercised too) actually returns priya-raman
// as a candidate for the freshly-extracted name priha-raman: the one
// concrete name-to-name case calibrated against openai/text-embedding-
// 3-small in .superpowers/sdd/2026-08-20-entity-resolution/progress.md
// (priya-raman/priha-raman = 0.2350, comfortably inside the 0.45 bound;
// the worst real variant measured, priyna-raman, was 0.3201 — still
// 0.13 of headroom below the bound). This assertion is deliberately
// about candidate recall alone, never about a merge outcome: an empty
// candidate list and a model declining to merge produce the same end
// state, and conflating the two is exactly what let the inert
// content-to-name version look correct.
func TestEntityCandidatesForNameFindsARealVariant(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	var provider, model, baseURL, apiKey string
	switch {
	case os.Getenv("OPENROUTER_API_KEY") != "":
		provider, model, apiKey = "openrouter", "openai/text-embedding-3-small", os.Getenv("OPENROUTER_API_KEY")
	case os.Getenv("OPENAI_API_KEY") != "":
		provider, model, baseURL, apiKey = "openai", "text-embedding-3-small", "https://api.openai.com/v1", os.Getenv("OPENAI_API_KEY")
	default:
		t.Skip("no embedding provider credentials (OPENROUTER_API_KEY or OPENAI_API_KEY) set; this test needs a REAL embedder, not fakeEmbedder — see the doc comment for why")
	}
	// 1536 matches the test database's migrated embedding width (schema
	// v9 at the time of writing), the same width every other test in
	// this file assumes via vec1536.
	emb, err := embed.New(provider, model, baseURL, apiKey, 1536)
	if err != nil {
		t.Fatalf("construct real embedder: %v", err)
	}

	st, scope := newGraphTestStore(t, "entity-real-embedder-test")
	ctx := context.Background()

	// Seed one entity exactly the way runGraph does on a real ADD_ENTITY:
	// upsert, then attach the name's own embedding (embedEntityName).
	vecs, err := emb.Embed(ctx, []string{"priya-raman"})
	if err != nil {
		t.Fatalf("embed priya-raman: %v", err)
	}
	if len(vecs) == 0 {
		t.Fatal("embedder returned no vector for priya-raman")
	}
	if err := st.UpsertEntity(ctx, &anamnesia.Entity{Scope: scope, Kind: "person", Name: "priya-raman", Embedding: vecs[0]}); err != nil {
		t.Fatalf("upsert priya-raman: %v", err)
	}

	// The real path, not the DI'd core: an actual Extractor, calling its
	// actual entityCandidatesForName method, so this also exercises
	// Cfg.applyDefaults() and the e.Store.NearestEntities method-value
	// binding, not only the recall algorithm underneath them.
	ex := &Extractor{Cfg: Config{GraphCandidateDistance: 0.45}, Store: st, Embedder: emb}
	got := ex.entityCandidatesForName(ctx, scope, "person", "priha-raman")
	if len(got) == 0 {
		t.Fatal("entityCandidatesForName found no candidates for priha-raman against a real embedder — recall is structurally broken, the same way content-to-name recall was: see the doc comment above")
	}
	found := false
	for _, m := range got {
		if m.Entity.Name == "priya-raman" {
			found = true
		}
	}
	if !found {
		t.Errorf("candidates for priha-raman did not include priya-raman: %v", got)
	}
}

// TestASameNameEntityOfADifferentKindIsNeverMergedAway is failure
// scenario A of the cross-kind review finding: one checkpoint mentions
// "rotterdam" both as a place and as the name of a migration project, so
// the model emits an ADD_ENTITY for each. An existing entity
// rotterdam-warehouse (kind project) is offered as a candidate to the
// PROJECT op only — the same-kind filter in entityCandidatesForName does
// its job — and the model affirms the merge. The place must still be
// created as its own entity: nothing the model said about a project can
// be applied to a place, and a wrong merge is irreversible and silent
// while a fork is visible and fixable.
//
// Measured against the real embedder, rotterdam/rotterdam-warehouse sit
// 0.2256 apart (.superpowers/sdd/2026-08-20-entity-resolution/
// progress.md), inside any usable candidate bound — so this is not a
// hypothetical pairing, it is the one the same-kind filter exists for.
func TestASameNameEntityOfADifferentKindIsNeverMergedAway(t *testing.T) {
	st, scope := newGraphTestStore(t, "entity-cross-kind-merge-test")
	ctx := context.Background()

	emb := &fakeEmbedder{Vecs: map[string][]float32{
		"rotterdam-warehouse": vec1536(1, 0, 0),
		"rotterdam":           vec1536(0.99, 0.01, 0),
	}}
	warehouse := &anamnesia.Entity{
		Scope: scope, Kind: "project", Name: "rotterdam-warehouse",
		Embedding: vec1536(1, 0, 0),
	}
	if err := st.UpsertEntity(ctx, warehouse); err != nil {
		t.Fatalf("upsert rotterdam-warehouse: %v", err)
	}

	src := &anamnesia.Source{Scope: scope, Kind: graphSourceKind, OccurredAt: time.Now().UTC(),
		RawContent: "The rotterdam migration project ships this quarter; the team in Rotterdam runs it."}
	if err := st.InsertSource(ctx, src); err != nil {
		t.Fatalf("insert source: %v", err)
	}

	fake := &fakeLLM{
		RawOps: marshalGraphOps(t, []graphOperation{
			{Op: "ADD_ENTITY", Kind: "place", Name: "rotterdam"},
			{Op: "ADD_ENTITY", Kind: "project", Name: "rotterdam"},
		}),
		// The verdict a real model would give: it was only ever asked
		// about the project (the place op has no candidate of its own
		// kind), and rotterdam-warehouse is that project's candidate.
		Verdicts: []identityVerdict{{Entity: "rotterdam", SameAs: warehouse.ID.String()}},
	}
	ex := &Extractor{Cfg: Config{ExtractGraph: true, GraphCandidateDistance: 0.45}, Store: st, Embedder: emb, LLM: fake}
	if _, err := ex.Run(ctx, src); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Calls != 2 {
		t.Fatalf("made %d model calls, want 2: the project op has a candidate, so the disambiguation call must run — this test proves nothing if it didn't", fake.Calls)
	}

	if _, err := st.LookupEntity(ctx, scope, "place", "rotterdam"); err != nil {
		t.Errorf("the place %q was not created: %v — a verdict about a project was applied to a place, merging a city into a warehouse", "rotterdam", err)
	}
	all, err := st.ListEntities(ctx, scope, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		names := make([]string, len(all))
		for i, a := range all {
			names[i] = a.Kind + "/" + a.Name
		}
		t.Errorf("ListEntities = %d, want 2 (project/rotterdam-warehouse and place/rotterdam); got %v", len(all), names)
	}
}

// TestTwoSameNameKindsBothLandWithTheirOwnMentions is failure scenario B
// of the same finding, and it needs no model error at all: the same two
// ops, no candidates anywhere (no embedder, so nothing is ever recalled
// and the disambiguation call never runs). Both entities are created
// either way — the (scope, kind, name) index sees to that — but every
// map keyed by bare name keeps only the last one, so an edge naming
// "rotterdam" resolves to whichever came second, and only that one gets
// an entity_mentions row. The other is permanently invisible to
// graphExpand, which joins on exactly those rows.
func TestTwoSameNameKindsBothLandWithTheirOwnMentions(t *testing.T) {
	st, scope := newGraphTestStore(t, "entity-cross-kind-mentions-test")
	ctx := context.Background()

	src := &anamnesia.Source{Scope: scope, Kind: graphSourceKind, OccurredAt: time.Now().UTC(),
		RawContent: "stock-reconciliation reads from rotterdam; the rotterdam project owns the rollout."}
	if err := st.InsertSource(ctx, src); err != nil {
		t.Fatalf("insert source: %v", err)
	}

	fake := &fakeLLM{RawOps: marshalGraphOps(t, []graphOperation{
		{Op: "ADD_ENTITY", Kind: "service", Name: "stock-reconciliation"},
		{Op: "ADD_ENTITY", Kind: "place", Name: "rotterdam"},
		{Op: "ADD_ENTITY", Kind: "project", Name: "rotterdam"},
		{Op: "ADD_EDGE", From: "stock-reconciliation", To: "rotterdam", Kind: "reads_from", Trust: 0.8},
	})}
	ex := &Extractor{Cfg: Config{ExtractGraph: true}, Store: st, LLM: fake}
	n, err := ex.Run(ctx, src)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Calls != 1 {
		t.Fatalf("made %d model calls, want 1: no embedder means no candidates, so the disambiguation call must not run", fake.Calls)
	}
	// Three entities, no edge: "rotterdam" names two entities of
	// different kinds this pass, and an ADD_EDGE endpoint carries no
	// kind, so the edge is dropped rather than pointed at whichever one
	// happened to be written last.
	if n != 3 {
		t.Errorf("executed = %d, want 3 (three entities, and an ambiguous edge dropped rather than guessed)", n)
	}
	all, err := st.ListEntities(ctx, scope, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("ListEntities = %d, want 3", len(all))
	}
	service, err := st.LookupEntity(ctx, scope, "service", "stock-reconciliation")
	if err != nil {
		t.Fatal(err)
	}
	_, edges, err := st.Neighbors(ctx, service.ID, []string{"reads_from"}, "out", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Errorf("an ambiguous endpoint produced %d edges anyway: an edge pointing at the wrong rotterdam would be believed downstream", len(edges))
	}

	// The part graphExpand depends on: EVERY entity this checkpoint
	// created carries a mention of the source, not just the last one to
	// claim its name.
	mentioned, err := st.EntitiesForSources(ctx, []uuid.UUID{src.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(mentioned) != 3 {
		names := make([]string, len(mentioned))
		for i, m := range mentioned {
			names[i] = m.Kind + "/" + m.Name
		}
		t.Errorf("EntitiesForSources = %d entities, want 3 — an entity with no mention row is permanently invisible to the graph walk; got %v", len(mentioned), names)
	}
}

// TestKindCaseDriftStillLandsOnOneEntity: nothing in the graph prompt or
// the operation schema constrains the CASE of "kind", so a model can
// write "Person" in one checkpoint and "person" in the next. Both halves
// of the machinery that keeps entities from forking are keyed on it —
// the (scope, kind, name) unique index, and the same-kind candidate
// filter — so unless kind is normalised the way name already is, the two
// spellings become two entities that can never be offered as each
// other's candidate. That is entity resolution going permanently,
// silently inert for that pair, with no log line to notice.
func TestKindCaseDriftStillLandsOnOneEntity(t *testing.T) {
	st, scope := newGraphTestStore(t, "entity-kind-case-drift-test")
	ctx := context.Background()

	emb := &fakeEmbedder{Vecs: map[string][]float32{"priya-raman": vec1536(1, 0, 0)}}
	newSource := func(content string) *anamnesia.Source {
		src := &anamnesia.Source{Scope: scope, Kind: graphSourceKind, OccurredAt: time.Now().UTC(), RawContent: content}
		if err := st.InsertSource(ctx, src); err != nil {
			t.Fatalf("insert source: %v", err)
		}
		return src
	}

	src1 := newSource("Priya Raman owns the nightly stock reconciliation job.")
	fake1 := &fakeLLM{RawOps: marshalGraphOps(t, []graphOperation{{Op: "ADD_ENTITY", Kind: "Person", Name: "Priya Raman"}})}
	ex1 := &Extractor{Cfg: Config{ExtractGraph: true, GraphCandidateDistance: 0.45}, Store: st, Embedder: emb, LLM: fake1}
	if _, err := ex1.Run(ctx, src1); err != nil {
		t.Fatalf("run session 1: %v", err)
	}

	src2 := newSource("Priya Raman is back from leave.")
	fake2 := &fakeLLM{RawOps: marshalGraphOps(t, []graphOperation{{Op: "ADD_ENTITY", Kind: "person", Name: "priya-raman"}})}
	ex2 := &Extractor{Cfg: Config{ExtractGraph: true, GraphCandidateDistance: 0.45}, Store: st, Embedder: emb, LLM: fake2}
	if _, err := ex2.Run(ctx, src2); err != nil {
		t.Fatalf("run session 2: %v", err)
	}
	if fake2.Calls != 2 {
		t.Errorf("session 2 made %d model calls, want 2: the entity session 1 created must be offered as a candidate, and a kind that differs only in case must not hide it", fake2.Calls)
	}

	all, err := st.ListEntities(ctx, scope, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		kinds := make([]string, len(all))
		for i, a := range all {
			kinds[i] = a.Kind + "/" + a.Name
		}
		t.Errorf("ListEntities = %d, want 1: %q and %q are the same kind; got %v", len(all), "Person", "person", kinds)
	}
}

// TestAVerdictIsAppliedToTheEntityItNames is the other half of the
// cross-kind fix: when BOTH same-name entities have candidates of their
// own kind, two questions go to the model under one name, and only the
// kind echoed back says which one an answer is about.
//
// The two subtests are the two outcomes that must follow. Kinded: the
// verdict lands on the project and only the project — verified by
// mutation, ignoring the verdict's kind makes that subtest fail.
// Kindless: it could be about either, and a verdict that cannot be
// pinned to one entity is dropped — both entities are created
// separately, the same fail-safe direction as a hallucinated same_as id,
// because a fork is visible and fixable while a wrong merge is neither.
//
// Honest limit on the kindless half: it does NOT isolate the ambiguity
// check in resolveIdentities, and cannot. Candidate recall filters by
// kind, so two same-name sets have disjoint candidate lists, and an id
// affirmed for one of them can never match the other's — the same_as id
// check alone already drops this verdict, and neutering the ambiguity
// check leaves this subtest green (checked by doing exactly that; not
// committed). No fixture can separate the two while recall stays
// kind-filtered, which is the point: the ambiguity check is the second,
// independent line of defence for the day something loosens that filter,
// exactly as the (scope, kind, name) index backs up the filter itself.
func TestAVerdictIsAppliedToTheEntityItNames(t *testing.T) {
	// Two existing entities, one per kind, each a candidate for the
	// extracted "rotterdam" of its own kind.
	setup := func(t *testing.T, prefix string) (*store.Store, anamnesia.Scope, *anamnesia.Entity, *fakeEmbedder, *anamnesia.Source) {
		t.Helper()
		st, scope := newGraphTestStore(t, prefix)
		ctx := context.Background()
		warehouse := &anamnesia.Entity{Scope: scope, Kind: "project", Name: "rotterdam-warehouse", Embedding: vec1536(0.98, 0.02, 0)}
		if err := st.UpsertEntity(ctx, warehouse); err != nil {
			t.Fatalf("upsert rotterdam-warehouse: %v", err)
		}
		if err := st.UpsertEntity(ctx, &anamnesia.Entity{Scope: scope, Kind: "place", Name: "rotterdam-centraal", Embedding: vec1536(0.97, 0.03, 0)}); err != nil {
			t.Fatalf("upsert rotterdam-centraal: %v", err)
		}
		src := &anamnesia.Source{Scope: scope, Kind: graphSourceKind, OccurredAt: time.Now().UTC(),
			RawContent: "The rotterdam project ships this quarter; the team meets in rotterdam."}
		if err := st.InsertSource(ctx, src); err != nil {
			t.Fatalf("insert source: %v", err)
		}
		emb := &fakeEmbedder{Vecs: map[string][]float32{"rotterdam": vec1536(1, 0, 0)}}
		return st, scope, warehouse, emb, src
	}
	ops := marshalGraphOps(t, []graphOperation{
		{Op: "ADD_ENTITY", Kind: "place", Name: "rotterdam"},
		{Op: "ADD_ENTITY", Kind: "project", Name: "rotterdam"},
	})

	t.Run("with the kind echoed back", func(t *testing.T) {
		st, scope, warehouse, emb, src := setup(t, "entity-verdict-kinded-test")
		ctx := context.Background()
		fake := &fakeLLM{
			RawOps:   ops,
			Verdicts: []identityVerdict{{Entity: "rotterdam", Kind: "project", SameAs: warehouse.ID.String()}},
		}
		ex := &Extractor{Cfg: Config{ExtractGraph: true, GraphCandidateDistance: 0.45}, Store: st, Embedder: emb, LLM: fake}
		if _, err := ex.Run(ctx, src); err != nil {
			t.Fatalf("run: %v", err)
		}
		if fake.Calls != 2 {
			t.Fatalf("made %d model calls, want 2: both ops have a candidate of their own kind, so the disambiguation call must run", fake.Calls)
		}
		if _, err := st.LookupEntity(ctx, scope, "place", "rotterdam"); err != nil {
			t.Errorf("the place %q was not created: %v — the verdict named the project", "rotterdam", err)
		}
		if _, err := st.LookupEntity(ctx, scope, "project", "rotterdam"); err == nil {
			t.Error("project/rotterdam was created as its own entity: the verdict said it is rotterdam-warehouse")
		}
		all, err := st.ListEntities(ctx, scope, "", 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 3 {
			t.Errorf("ListEntities = %d, want 3 (the two existing entities plus place/rotterdam)", len(all))
		}
	})

	t.Run("without a kind, when the name alone is ambiguous", func(t *testing.T) {
		st, scope, warehouse, emb, src := setup(t, "entity-verdict-kindless-test")
		ctx := context.Background()
		fake := &fakeLLM{
			RawOps:   ops,
			Verdicts: []identityVerdict{{Entity: "rotterdam", SameAs: warehouse.ID.String()}},
		}
		ex := &Extractor{Cfg: Config{ExtractGraph: true, GraphCandidateDistance: 0.45}, Store: st, Embedder: emb, LLM: fake}
		if _, err := ex.Run(ctx, src); err != nil {
			t.Fatalf("run: %v", err)
		}
		all, err := st.ListEntities(ctx, scope, "", 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 4 {
			names := make([]string, len(all))
			for i, a := range all {
				names[i] = a.Kind + "/" + a.Name
			}
			t.Errorf("ListEntities = %d, want 4: the verdict names two entities at once, so it merges neither; got %v", len(all), names)
		}
	})
}

// TestEntityPropsSurviveARedeclarationWithoutProps guards the normal
// case: a checkpoint re-declares an entity an earlier checkpoint already
// described. Re-declaration carries whatever the model happened to say
// this time, which is usually no props at all — and a wholesale replace
// would silently drop the attribute the first checkpoint recorded, with
// no trace entry and no way to notice.
//
// The documented rule this asserts: props merge per key, the newer value
// wins, and an absent key leaves what was there alone.
func TestEntityPropsSurviveARedeclarationWithoutProps(t *testing.T) {
	st, scope := newGraphTestStore(t, "entity-props-test")
	ctx := context.Background()

	run := func(content string, ops []graphOperation) {
		t.Helper()
		src := &anamnesia.Source{Scope: scope, Kind: graphSourceKind, OccurredAt: time.Now().UTC(), RawContent: content}
		if err := st.InsertSource(ctx, src); err != nil {
			t.Fatalf("insert source: %v", err)
		}
		ex := &Extractor{Cfg: Config{ExtractGraph: true}, Store: st, LLM: &fakeLLM{RawOps: marshalGraphOps(t, ops)}}
		if _, err := ex.Run(ctx, src); err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	props := func() map[string]any {
		t.Helper()
		ent, err := st.LookupEntity(ctx, scope, "service", "stock-reconciliation")
		if err != nil {
			t.Fatalf("lookup entity: %v", err)
		}
		return ent.Props
	}

	run("the stock-reconciliation service runs as a nightly job against the warehouse.",
		[]graphOperation{{Op: "ADD_ENTITY", Kind: "service", Name: "stock-reconciliation",
			Props: map[string]any{"role": "nightly job", "owner": "ops"}}})
	if got := props(); got["role"] != "nightly job" {
		t.Fatalf("props after the first checkpoint = %v, want role recorded; test setup is invalid", got)
	}

	// Re-declared with no props at all — the common case.
	run("stock-reconciliation came up again today, nothing new about it.",
		[]graphOperation{{Op: "ADD_ENTITY", Kind: "service", Name: "stock-reconciliation"}})
	if got := props(); got["role"] != "nightly job" || got["owner"] != "ops" {
		t.Fatalf("props = %v after a re-declaration carrying none; want both keys intact", got)
	}

	// Re-declared with one new key and one changed key: the newer value
	// wins per key, and the key nobody mentioned survives.
	run("stock-reconciliation now runs hourly and is owned by the platform team.",
		[]graphOperation{{Op: "ADD_ENTITY", Kind: "service", Name: "stock-reconciliation",
			Props: map[string]any{"owner": "platform", "schedule": "hourly"}}})
	got := props()
	if got["owner"] != "platform" {
		t.Errorf("props[owner] = %v, want the newer value to win", got["owner"])
	}
	if got["schedule"] != "hourly" {
		t.Errorf("props[schedule] = %v, want the new key added", got["schedule"])
	}
	if got["role"] != "nightly job" {
		t.Errorf("props[role] = %v, want the key nobody re-declared left alone", got["role"])
	}
}

// TestGraphTraceCountsMentionsAndFlagsMissingSegmentSources: an empty
// segment_source_ids list is the exact defect that already shipped on
// this branch once (docs/superpowers/specs/2026-08-19-the-graph-bridge-
// is-broken.md). Nothing observed it: the trace's graph step reported
// entities and edges but never mentions, and the helper only warned on a
// wrong SHAPE, never on an absent or empty list. The channel could go
// inert again and every trace would still read "ok".
func TestGraphTraceCountsMentionsAndFlagsMissingSegmentSources(t *testing.T) {
	st, scope := newGraphTestStore(t, "graph-trace-mentions")
	ctx := context.Background()

	ops := []graphOperation{
		{Op: "ADD_ENTITY", Kind: "service", Name: "stock-reconciliation"},
		{Op: "ADD_ENTITY", Kind: "site", Name: "rotterdam-warehouse"},
		{Op: "ADD_EDGE", From: "stock-reconciliation", To: "rotterdam-warehouse", Kind: "reads_from"},
	}
	run := func(meta map[string]any) (*activity.Trace, string) {
		t.Helper()
		src := &anamnesia.Source{
			Scope: scope, Kind: graphSourceKind, OccurredAt: time.Now().UTC(),
			RawContent: "the stock-reconciliation service reads from the rotterdam warehouse nightly.",
			Metadata:   meta,
		}
		if err := st.InsertSource(ctx, src); err != nil {
			t.Fatalf("insert source: %v", err)
		}
		loaded, err := st.GetSource(ctx, src.ID)
		if err != nil {
			t.Fatalf("reload source: %v", err)
		}
		var logs bytes.Buffer
		rec := activity.New(4)
		ex := &Extractor{
			Cfg:   Config{ExtractGraph: true},
			Store: st,
			LLM:   &fakeLLM{RawOps: marshalGraphOps(t, ops)},
			Log:   slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
			// A recorder per run, so onlyTrace sees exactly this one.
			Activity: rec,
		}
		if _, err := ex.Run(ctx, loaded); err != nil {
			t.Fatalf("run: %v", err)
		}
		return onlyTrace(t, rec), logs.String()
	}
	graphStep := func(tr *activity.Trace) map[string]any {
		t.Helper()
		for _, s := range tr.Steps {
			if s.Name == "graph" {
				return s.Detail
			}
		}
		t.Fatalf("no graph step in the trace: steps = %v", stepNames(tr.Steps))
		return nil
	}

	// No segment_source_ids at all: mentions land only on the graph
	// source, which no search hit ever carries. Nothing is broken enough
	// to fail the pass, so the warning and the count are the only
	// evidence there is.
	tr, logs := run(nil)
	detail := graphStep(tr)
	if detail["mentions_recorded"] != 2 {
		t.Errorf("mentions_recorded = %v, want 2 (two entities against the graph source alone)", detail["mentions_recorded"])
	}
	if detail["segment_sources"] != 0 {
		t.Errorf("segment_sources = %v, want 0", detail["segment_sources"])
	}
	if !strings.Contains(logs, "segment_source_ids") {
		t.Errorf("no warning about the missing segment_source_ids; logs = %q", logs)
	}

	// The healthy shape: two segments, so every entity is mentioned
	// against both of them plus the graph source.
	seg1 := &anamnesia.Source{Scope: scope, Kind: "claude-session", OccurredAt: time.Now().UTC(), RawContent: "segment one, long enough to be a source."}
	seg2 := &anamnesia.Source{Scope: scope, Kind: "claude-session", OccurredAt: time.Now().UTC(), RawContent: "segment two, long enough to be a source."}
	for _, seg := range []*anamnesia.Source{seg1, seg2} {
		if err := st.InsertSource(ctx, seg); err != nil {
			t.Fatalf("insert segment: %v", err)
		}
	}
	tr, logs = run(map[string]any{"segment_source_ids": []string{seg1.ID.String(), seg2.ID.String()}})
	detail = graphStep(tr)
	if detail["mentions_recorded"] != 6 {
		t.Errorf("mentions_recorded = %v, want 6 (two entities against the graph source and both segments)", detail["mentions_recorded"])
	}
	if detail["segment_sources"] != 2 {
		t.Errorf("segment_sources = %v, want 2", detail["segment_sources"])
	}
	if strings.Contains(logs, "segment_source_ids") {
		t.Errorf("a healthy checkpoint warned about its segment_source_ids anyway; logs = %q", logs)
	}
}
