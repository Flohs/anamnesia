package extract

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

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
