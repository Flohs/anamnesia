package retrieval

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

func hitIDs(hits []anamnesia.SearchHit) []uuid.UUID {
	out := make([]uuid.UUID, len(hits))
	for i, h := range hits {
		out[i] = h.ID()
	}
	return out
}

func sameOrder(a, b []anamnesia.SearchHit) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID() != b[i].ID() {
			return false
		}
	}
	return true
}

// TestAnEmptyGraphChangesNothing is the regression that matters most: the
// graph channel ships enabled on the read side (Search defaults
// GraphSeedN/GraphFanout/GraphK the same way it defaults VectorK/LexicalK,
// rather than requiring every existing caller to opt in — see the
// Query.GraphSeedN doc in retrieval.go), and every install in the world
// today has an empty graph. Results must be unaffected.
//
// This does NOT compare GraphSeedN:0 against GraphSeedN-unset the way the
// original sketch for this test proposed. Search defaults any
// non-positive GraphSeedN to 5, so those two values collapse to the exact
// same Query and the comparison would be tautological — it would pass
// even if graphExpand always fired. Instead this: (a) calls graphExpand
// directly to prove its own empty-graph guard is what fires — sourced
// facts with entity_mentions rows for neither source, so
// EntitiesForSources genuinely runs and genuinely returns nothing, per
// the brief's "step 2 returning no entities should end it before any
// further query" — and (b) compares two different *positive* seed counts
// (1 and the default 5) through the public Search API, which exercises a
// genuinely different fused[:seedN] slice inside graphExpand while still
// proving the channel is a no-op end to end.
func TestAnEmptyGraphChangesNothing(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	handle := "retrieval-empty-graph-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), handle) })
	scope := anamnesia.Scope{UserID: uid}

	// Two sourced facts, both matching the query, at different lexical
	// ranks. Nothing in entities or entity_mentions mentions either
	// source: the graph has rows to query against (so EntitiesForSources
	// really runs) but nothing for it to find.
	src1 := &anamnesia.Source{Scope: scope, Kind: "claude-session-graph", RawContent: "x"}
	if err := st.InsertSource(ctx, src1); err != nil {
		t.Fatal(err)
	}
	src2 := &anamnesia.Source{Scope: scope, Kind: "claude-session-graph", RawContent: "x"}
	if err := st.InsertSource(ctx, src2); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "quokka-primary", SourceID: &src1.ID,
		Value: map[string]any{"note": "quokka quokka quokka rollout, the quokka effort"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "quokka-secondary", SourceID: &src2.ID,
		Value: map[string]any{"note": "a passing mention of quokka"},
	}); err != nil {
		t.Fatal(err)
	}

	eng := &Engine{Store: st}
	base, err := eng.Search(ctx, Query{Scope: scope, Text: "quokka"})
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != 2 || base[0].Fact == nil || base[0].Fact.Key != "quokka-primary" ||
		base[1].Fact == nil || base[1].Fact.Key != "quokka-secondary" {
		t.Fatalf("hits = %v, want exactly the two facts, higher-frequency match first, nothing the graph invented", hitIDs(base))
	}

	// Direct, white-box check that the empty-graph guard inside
	// graphExpand is what fires. Production change that would make this
	// fail: EntitiesForSources being skipped/short-circuited before it
	// actually queries (masking a real bug elsewhere), or the "found no
	// entities" case falling through into the walk instead of returning
	// immediately.
	direct, err := eng.graphExpand(ctx, Query{
		Scope:      scope,
		Domains:    []anamnesia.Domain{anamnesia.DomainFact, anamnesia.DomainExperience},
		GraphSeedN: 5, GraphFanout: 10, GraphK: 20,
	}, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(direct) != 0 {
		t.Errorf("graphExpand returned %d hits over an empty graph, want none", len(direct))
	}

	// A different, still-positive seed count must not change the
	// end-to-end outcome: it takes a genuinely different fused[:seedN]
	// slice into graphExpand, and both must still find nothing. Production
	// change that would make this fail: graphExpand contributing phantom
	// hits, dropping real ones, or erroring differently depending on how
	// many seeds it's handed.
	//
	// This does NOT probe the unconditional-recompute-and-resort mistake
	// named in retrieval.go's comment on this section (skipping the
	// `len(graphHits) > 0` guard) — verified by deliberately introducing
	// that exact mutation and rerunning this test 20x: it still passed
	// every time, because with two facts that don't tie in score, a
	// pointless second sort.Slice pass converges on the same order as the
	// first regardless of Go's randomized map iteration. Reliably catching
	// that mutation would need two hits with an exactly tied RRF score,
	// which isn't safe to construct here: the fused-only sort above has
	// the identical instability already, with or without this channel, so
	// a tie would make even the *correct* implementation flip order
	// between calls. That's a preexisting property of Search's RRF fusion
	// this task didn't introduce and isn't the graph channel's job to fix.
	alt, err := eng.Search(ctx, Query{Scope: scope, Text: "quokka", GraphSeedN: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !sameOrder(base, alt) {
		t.Errorf("GraphSeedN 1 vs default 5 over an empty graph: got different results\n default: %v\n seedN=1: %v", hitIDs(base), hitIDs(alt))
	}
}

// TestTheGraphSurfacesARowNeitherSearchFinds is the whole claim of this
// design, in one test: a row reachable only by walking an edge, that
// vector and lexical search both miss. (No embedder is wired here, so
// "both" reduces to "lexical": Search's own qvec stays nil throughout.)
func TestTheGraphSurfacesARowNeitherSearchFinds(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	handle := "retrieval-graph-surface-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), handle) })
	scope := anamnesia.Scope{UserID: uid}

	// Two sources whose text does not overlap at all.
	src1 := &anamnesia.Source{Scope: scope, Kind: "claude-session-graph", RawContent: "x"}
	if err := st.InsertSource(ctx, src1); err != nil {
		t.Fatal(err)
	}
	src2 := &anamnesia.Source{Scope: scope, Kind: "claude-session-graph", RawContent: "x"}
	if err := st.InsertSource(ctx, src2); err != nil {
		t.Fatal(err)
	}

	exp1 := &anamnesia.Experience{
		Scope: scope, Kind: anamnesia.ExperienceCase, SourceID: &src1.ID,
		Title: "aardvark migration", Body: "Notes on the aardvark migration rollout.",
	}
	if err := st.RecordExperience(ctx, exp1); err != nil {
		t.Fatal(err)
	}
	exp2 := &anamnesia.Experience{
		Scope: scope, Kind: anamnesia.ExperienceCase, SourceID: &src2.ID,
		Title: "zebra patrol", Body: "Notes on the zebra crossing patrol logs.",
	}
	if err := st.RecordExperience(ctx, exp2); err != nil {
		t.Fatal(err)
	}

	// One entity per source, one edge between them.
	ent1 := &anamnesia.Entity{Scope: scope, Kind: "topic", Name: "aardvark-project"}
	if err := st.UpsertEntity(ctx, ent1); err != nil {
		t.Fatal(err)
	}
	ent2 := &anamnesia.Entity{Scope: scope, Kind: "topic", Name: "zebra-project"}
	if err := st.UpsertEntity(ctx, ent2); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMention(ctx, ent1.ID, src1.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMention(ctx, ent2.ID, src2.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateEdge(ctx, &anamnesia.Edge{From: ent1.ID, To: ent2.ID, Kind: "related_to", Trust: 0.9}); err != nil {
		t.Fatal(err)
	}

	eng := &Engine{Store: st}

	// Prove the premise: lexical search for "aardvark" alone does not
	// find the zebra experience.
	lex, err := eng.lexicalExperiences(ctx, scope, "aardvark", 40, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range lex {
		if h.Experience != nil && h.Experience.ID == exp2.ID {
			t.Fatal("lexical search for \"aardvark\" already found the zebra experience directly; this fixture doesn't isolate what it's supposed to")
		}
	}

	hits, err := eng.Search(ctx, Query{Scope: scope, Text: "aardvark"})
	if err != nil {
		t.Fatal(err)
	}
	var foundExp1, foundExp2 bool
	for _, h := range hits {
		if h.Experience == nil {
			continue
		}
		switch h.Experience.ID {
		case exp1.ID:
			foundExp1 = true
		case exp2.ID:
			foundExp2 = true
		}
	}
	if !foundExp1 {
		t.Errorf("hits = %v, want the directly-matching aardvark experience", hitIDs(hits))
	}
	if !foundExp2 {
		t.Errorf("hits = %v, want the zebra experience surfaced through the aardvark-project -> zebra-project edge; the graph channel isn't doing anything", hitIDs(hits))
	}
}
