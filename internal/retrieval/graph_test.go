package retrieval

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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
// original sketch for this test proposed. Search's zero value and unset
// are the same Go value, and Search defaults exactly that to 5 (a
// negative value is what actually disables the channel now — see the
// Query.GraphSeedN doc), so GraphSeedN:0 and GraphSeedN-unset collapse to
// the identical Query and the comparison would be tautological — it
// would pass even if graphExpand always fired. Instead this: (a) calls
// graphExpand directly with real sourced facts and an empty
// entities/entity_mentions table, to show the walk genuinely finds
// nothing rather than assuming it — real EntitiesForSources call, real
// zero rows back, not the guard itself being load-bearing (ranging over
// zero entities already makes zero further Neighbors calls on its own;
// see the comment where that guard lives in graph.go) — and (b) compares
// two different *positive* seed counts (1 and the default 5) through the
// public Search API, which exercises a genuinely different
// fused[:seedN] slice inside graphExpand while still proving the channel
// is a no-op end to end.
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

	// Direct, white-box check that walking an empty graph genuinely finds
	// nothing: EntitiesForSources really runs (real sourced facts, real
	// entity_mentions table, nothing in it for these two sources) and the
	// walk it feeds produces zero candidates. Production change that
	// would make this fail: graphExpand returning phantom hits, or
	// erroring, when nothing in entities/entity_mentions/edges matches.
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

	// A negative GraphSeedN must genuinely turn the channel off, using
	// this same fixture where "on" is known to surface exp2: this is the
	// real kill switch (Search only replaces GraphSeedN == 0, so -1
	// survives into graphExpand's own seedN<=0 guard), not the impossible
	// "0 disables" this task's brief originally asked for.
	off, err := eng.Search(ctx, Query{Scope: scope, Text: "aardvark", GraphSeedN: -1})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range off {
		if h.Experience != nil && h.Experience.ID == exp2.ID {
			t.Errorf("hits = %v with GraphSeedN -1, want the graph channel off and the zebra experience absent", hitIDs(off))
		}
	}
}

// TestGraphWalkStaysInsideItsTenant: anamnesia_graph_edge takes raw
// entity UUIDs with no scope check, so nothing today stops a cross-user
// edge from being created, and Store.Neighbors/SourcesForEntities (Task
// 1-3, no scope parameter of their own) will traverse it just as readily
// as an in-tenant one. This fixture mirrors
// TestTheGraphSurfacesARowNeitherSearchFinds exactly, except the two
// experiences belong to two different users and the edge crosses between
// them.
//
// graphExpand has two independent scope predicates (entitiesInScope/
// inScope filtering the walk itself, and the user_id filter already in
// hitsForSources' SQL), but for THIS fixture they turn out to be fully
// redundant with each other: entB is filtered out as a neighbour by the
// walk-level predicate before hitsForSources is ever asked about it, so
// removing either predicate alone (checked by deliberately neutering
// each, one at a time, and rerunning — not committed) still leaves the
// other one blocking the leak, and this test stays green either way. That
// is not a wasted test — it is real defense in depth, and it is still the
// scenario the review named — but it means this specific test cannot be
// the thing that proves hitsForSources' filter matters. See
// TestGraphWalkKeepsABadMentionInsideItsTenant below for a fixture where
// it's the only thing that does, and TestInScope for a direct,
// DB-free check of the walk-level predicate's own logic.
func TestGraphWalkStaysInsideItsTenant(t *testing.T) {
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
	handleA := "retrieval-tenant-a-" + uuid.NewString()[:8]
	uidA, err := st.EnsureUser(ctx, handleA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), handleA) })
	handleB := "retrieval-tenant-b-" + uuid.NewString()[:8]
	uidB, err := st.EnsureUser(ctx, handleB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), handleB) })
	scopeA := anamnesia.Scope{UserID: uidA}
	scopeB := anamnesia.Scope{UserID: uidB}

	srcA := &anamnesia.Source{Scope: scopeA, Kind: "claude-session-graph", RawContent: "x"}
	if err := st.InsertSource(ctx, srcA); err != nil {
		t.Fatal(err)
	}
	srcB := &anamnesia.Source{Scope: scopeB, Kind: "claude-session-graph", RawContent: "x"}
	if err := st.InsertSource(ctx, srcB); err != nil {
		t.Fatal(err)
	}

	expA := &anamnesia.Experience{
		Scope: scopeA, Kind: anamnesia.ExperienceCase, SourceID: &srcA.ID,
		Title: "sunfish migration", Body: "Notes on the sunfish migration rollout.",
	}
	if err := st.RecordExperience(ctx, expA); err != nil {
		t.Fatal(err)
	}
	expB := &anamnesia.Experience{
		Scope: scopeB, Kind: anamnesia.ExperienceCase, SourceID: &srcB.ID,
		Title: "otter patrol", Body: "Notes on the otter crossing patrol logs.",
	}
	if err := st.RecordExperience(ctx, expB); err != nil {
		t.Fatal(err)
	}

	entA := &anamnesia.Entity{Scope: scopeA, Kind: "topic", Name: "sunfish-project"}
	if err := st.UpsertEntity(ctx, entA); err != nil {
		t.Fatal(err)
	}
	entB := &anamnesia.Entity{Scope: scopeB, Kind: "topic", Name: "otter-project"}
	if err := st.UpsertEntity(ctx, entB); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMention(ctx, entA.ID, srcA.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMention(ctx, entB.ID, srcB.ID); err != nil {
		t.Fatal(err)
	}
	// The cross-tenant edge CreateEdge doesn't stop: entA (user A) to
	// entB (user B).
	if err := st.CreateEdge(ctx, &anamnesia.Edge{From: entA.ID, To: entB.ID, Kind: "related_to", Trust: 0.9}); err != nil {
		t.Fatal(err)
	}

	eng := &Engine{Store: st}
	hits, err := eng.Search(ctx, Query{Scope: scopeA, Text: "sunfish"})
	if err != nil {
		t.Fatal(err)
	}
	var foundExpA, leakedExpB bool
	for _, h := range hits {
		if h.Experience == nil {
			continue
		}
		switch h.Experience.ID {
		case expA.ID:
			foundExpA = true
		case expB.ID:
			leakedExpB = true
		}
	}
	if !foundExpA {
		t.Fatalf("hits = %v, want user A's own sunfish experience", hitIDs(hits))
	}
	if leakedExpB {
		t.Errorf("hits = %v, want user B's experience absent: it leaked across the cross-tenant edge", hitIDs(hits))
	}
}

// TestGraphWalkKeepsABadMentionInsideItsTenant isolates hitsForSources'
// own user_id filter as the thing that matters, by using a fixture the
// walk-level scope predicate cannot help with at all: entC below is a
// genuinely user-A-scoped entity (no edge crosses a tenant boundary, so
// entitiesInScope/inScope has nothing to object to), but it carries a
// mention linking it to a source owned by user B — RecordMention (Task
// 1-3) takes raw UUIDs with no scope check either, so nothing stops that
// mention existing (a bad extraction, a bug, anything). Only
// hitsForSources' own user_id filter, applied to the row it fetches, can
// catch this. Verified by mutation: neutering that filter (replacing
// "user_id = $2" with an always-true predicate that still binds $2, so
// the query stays valid SQL) makes this test fail; not committed.
func TestGraphWalkKeepsABadMentionInsideItsTenant(t *testing.T) {
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
	handleA := "retrieval-tenant-bad-mention-a-" + uuid.NewString()[:8]
	uidA, err := st.EnsureUser(ctx, handleA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), handleA) })
	handleB := "retrieval-tenant-bad-mention-b-" + uuid.NewString()[:8]
	uidB, err := st.EnsureUser(ctx, handleB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), handleB) })
	scopeA := anamnesia.Scope{UserID: uidA}
	scopeB := anamnesia.Scope{UserID: uidB}

	srcA := &anamnesia.Source{Scope: scopeA, Kind: "claude-session-graph", RawContent: "x"}
	if err := st.InsertSource(ctx, srcA); err != nil {
		t.Fatal(err)
	}
	srcB := &anamnesia.Source{Scope: scopeB, Kind: "claude-session-graph", RawContent: "x"}
	if err := st.InsertSource(ctx, srcB); err != nil {
		t.Fatal(err)
	}

	expA := &anamnesia.Experience{
		Scope: scopeA, Kind: anamnesia.ExperienceCase, SourceID: &srcA.ID,
		Title: "puffin migration", Body: "Notes on the puffin migration rollout.",
	}
	if err := st.RecordExperience(ctx, expA); err != nil {
		t.Fatal(err)
	}
	expB := &anamnesia.Experience{
		Scope: scopeB, Kind: anamnesia.ExperienceCase, SourceID: &srcB.ID,
		Title: "otter patrol", Body: "Notes on the otter crossing patrol logs.",
	}
	if err := st.RecordExperience(ctx, expB); err != nil {
		t.Fatal(err)
	}

	// entA (seed) -> entC (neighbour), both genuinely owned by user A.
	entA := &anamnesia.Entity{Scope: scopeA, Kind: "topic", Name: "puffin-project"}
	if err := st.UpsertEntity(ctx, entA); err != nil {
		t.Fatal(err)
	}
	entC := &anamnesia.Entity{Scope: scopeA, Kind: "topic", Name: "puffin-related"}
	if err := st.UpsertEntity(ctx, entC); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMention(ctx, entA.ID, srcA.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateEdge(ctx, &anamnesia.Edge{From: entA.ID, To: entC.ID, Kind: "related_to", Trust: 0.9}); err != nil {
		t.Fatal(err)
	}
	// The bad mention: entC (user A's own entity) mentioning a source
	// that belongs to user B.
	if err := st.RecordMention(ctx, entC.ID, srcB.ID); err != nil {
		t.Fatal(err)
	}

	eng := &Engine{Store: st}
	hits, err := eng.Search(ctx, Query{Scope: scopeA, Text: "puffin"})
	if err != nil {
		t.Fatal(err)
	}
	var foundExpA, leakedExpB bool
	for _, h := range hits {
		if h.Experience == nil {
			continue
		}
		switch h.Experience.ID {
		case expA.ID:
			foundExpA = true
		case expB.ID:
			leakedExpB = true
		}
	}
	if !foundExpA {
		t.Fatalf("hits = %v, want user A's own puffin experience", hitIDs(hits))
	}
	if leakedExpB {
		t.Errorf("hits = %v, want user B's experience absent: it leaked through a same-tenant entity's mention of a foreign source", hitIDs(hits))
	}
}

// TestInScope is a direct, DB-free check of the walk-level scope
// predicate's own logic (graph.go). It has no content-observable failure
// mode of its own in either DB-backed test above — both find that
// hitsForSources' user_id filter alone is enough to keep the specific
// fixtures they construct safe — so this is the test that actually
// constrains entitiesInScope/inScope: it is what would catch that logic
// being wrong or deleted, even though no single Search() call in this
// file can.
func TestInScope(t *testing.T) {
	userA, userB := uuid.New(), uuid.New()
	projX, projY := uuid.New(), uuid.New()

	cases := []struct {
		name      string
		candidate anamnesia.Scope
		want      anamnesia.Scope
		in        bool
	}{
		{"same user, neither scoped to a project", anamnesia.Scope{UserID: userA}, anamnesia.Scope{UserID: userA}, true},
		{"different user", anamnesia.Scope{UserID: userB}, anamnesia.Scope{UserID: userA}, false},
		{"user-level query sees a project-scoped candidate too", anamnesia.Scope{UserID: userA, ProjectID: &projX}, anamnesia.Scope{UserID: userA}, true},
		{"project-scoped query sees a user-level candidate", anamnesia.Scope{UserID: userA}, anamnesia.Scope{UserID: userA, ProjectID: &projX}, true},
		{"same user, same project", anamnesia.Scope{UserID: userA, ProjectID: &projX}, anamnesia.Scope{UserID: userA, ProjectID: &projX}, true},
		{"same user, different project", anamnesia.Scope{UserID: userA, ProjectID: &projX}, anamnesia.Scope{UserID: userA, ProjectID: &projY}, false},
		{"matching project id, different user", anamnesia.Scope{UserID: userB, ProjectID: &projX}, anamnesia.Scope{UserID: userA, ProjectID: &projX}, false},
	}
	for _, c := range cases {
		if got := inScope(c.candidate, c.want); got != c.in {
			t.Errorf("%s: inScope(%+v, %+v) = %v, want %v", c.name, c.candidate, c.want, got, c.in)
		}
	}
}

// TestASlowGraphWalkDegradesInsteadOfEliminatingRetrieval: the comment
// above graphExpand's call site in retrieval.go promises that "a slow or
// broken graph mid-walk must degrade retrieval, not turn a working one
// into no memory injected at all". Broken is covered by the err != nil
// path; SLOW is what this test is about. The retrieve hook gives the
// whole request 2.5s (cmd/anamnesia/hook.go) and handleRetrieve runs
// Search twice, so a walk that just keeps waiting eats the deadline, the
// handler never writes a response, and the prompt gets no memory at all
// — the opposite of degrading.
//
// The fixture is TestTheGraphSurfacesARowNeitherSearchFinds', where the
// graph channel is known to contribute exp2 and nothing else finds it,
// with one addition: an ACCESS EXCLUSIVE lock on `edges`, which makes
// every plain SELECT against that table wait. Store.Neighbors is the only
// thing in the read path that touches edges, so this blocks precisely the
// per-seed-entity round trips the walk makes — for real, through pgx,
// not by simulating a slow store. The lock is released the moment Search
// returns, so it holds for about one graph budget; a Search that ignores
// its budget is bounded by the select below rather than hanging the
// suite.
func TestASlowGraphWalkDegradesInsteadOfEliminatingRetrieval(t *testing.T) {
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
	handle := "retrieval-slow-graph-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), handle) })
	scope := anamnesia.Scope{UserID: uid}

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
		Title: "narwhal migration", Body: "Notes on the narwhal migration rollout.",
	}
	if err := st.RecordExperience(ctx, exp1); err != nil {
		t.Fatal(err)
	}
	exp2 := &anamnesia.Experience{
		Scope: scope, Kind: anamnesia.ExperienceCase, SourceID: &src2.ID,
		Title: "ibex patrol", Body: "Notes on the ibex crossing patrol logs.",
	}
	if err := st.RecordExperience(ctx, exp2); err != nil {
		t.Fatal(err)
	}
	ent1 := &anamnesia.Entity{Scope: scope, Kind: "topic", Name: "narwhal-project"}
	if err := st.UpsertEntity(ctx, ent1); err != nil {
		t.Fatal(err)
	}
	ent2 := &anamnesia.Entity{Scope: scope, Kind: "topic", Name: "ibex-project"}
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

	// Shrink the budget for this test. The lock below is ACCESS EXCLUSIVE
	// on a table every other package's DB tests also touch, and they run
	// in parallel against one database — so the lock is held for exactly
	// one budget, and a smaller budget is a shorter stall for everyone
	// else. The property under test is "the walk gives up and retrieval
	// still returns", which does not depend on how long the giving-up
	// takes.
	restore := graphBudget
	graphBudget = 20 * time.Millisecond
	t.Cleanup(func() { graphBudget = restore })

	// Block the walk. lock_timeout keeps this from waiting forever if
	// another package's DB test happens to hold a conflicting lock on the
	// shared test database at this moment.
	conn, err := st.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '10s'"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE edges IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("could not lock the edges table to block the graph walk: %v", err)
	}

	type result struct {
		hits []anamnesia.SearchHit
		err  error
		took time.Duration
	}
	done := make(chan result, 1)
	eng := &Engine{Store: st}
	go func() {
		started := time.Now()
		hits, err := eng.Search(ctx, Query{Scope: scope, Text: "narwhal"})
		done <- result{hits: hits, err: err, took: time.Since(started)}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		// Still waiting on the locked table: the walk has no budget of
		// its own and is spending the caller's. Unblock it so the suite
		// can finish, then report.
		_ = tx.Rollback(context.Background())
		got = <-done
		t.Fatalf("Search took %s against a blocked graph walk: the walk has no budget of its own, so it spends the whole 2.5s retrieve deadline and the prompt gets no memory at all", got.took)
	}
	if got.err != nil {
		t.Fatalf("Search returned an error (%v) when only the graph channel was slow: vector and lexical results were already computed and must still be returned", got.err)
	}

	var foundExp1, foundExp2 bool
	for _, h := range got.hits {
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
		t.Errorf("hits = %v after %s, want the directly-matching narwhal experience: a slow graph must degrade retrieval, not eliminate it", hitIDs(got.hits), got.took)
	}
	if foundExp2 {
		t.Errorf("hits = %v, want the graph-only ibex experience absent: the walk was blocked, so it cannot have contributed", hitIDs(got.hits))
	}
}

// trustWalk is the fixture both ordering tests below share: one seed
// source, and three neighbour entities reachable from it over edges of
// clearly different trust, each mentioning one source with one
// experience on it.
//
// The row-level trusts run the other way round on purpose. Extraction
// stamps essentially every row 0.7 (internal/extract/extract.go), so a
// fixture where row trust and edge trust agree would be green whichever
// signal the walk actually ordered on. Here the weakest edge carries the
// strongest row, so ordering by the row's own trust — which is what
// graphExpand did before the walk kept hold of which neighbour reached
// which source — puts exactly the wrong row first. The three
// experiences are also inserted weakest-edge-first, so a plan that keeps
// scan order cannot accidentally look right either.
type trustWalk struct {
	eng   *Engine
	scope anamnesia.Scope
	seed  []anamnesia.SearchHit
	// The three graph-reachable experiences, by the trust of the edge
	// that reaches them: 0.9, 0.5, 0.2.
	high, mid, low uuid.UUID
	label          map[uuid.UUID]string
}

func newTrustWalk(t *testing.T, name string) trustWalk {
	t.Helper()
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
	handle := name + "-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), handle) })
	scope := anamnesia.Scope{UserID: uid}

	source := func() *anamnesia.Source {
		src := &anamnesia.Source{Scope: scope, Kind: "claude-session-graph", RawContent: "x"}
		if err := st.InsertSource(ctx, src); err != nil {
			t.Fatal(err)
		}
		return src
	}
	experience := func(src *anamnesia.Source, title string, trust float32) *anamnesia.Experience {
		exp := &anamnesia.Experience{
			Scope: scope, Kind: anamnesia.ExperienceCase, SourceID: &src.ID,
			Title: title, Body: "Notes on the " + title + ".", Trust: trust,
		}
		if err := st.RecordExperience(ctx, exp); err != nil {
			t.Fatal(err)
		}
		return exp
	}
	entity := func(name string, src *anamnesia.Source) *anamnesia.Entity {
		ent := &anamnesia.Entity{Scope: scope, Kind: "topic", Name: name}
		if err := st.UpsertEntity(ctx, ent); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordMention(ctx, ent.ID, src.ID); err != nil {
			t.Fatal(err)
		}
		return ent
	}

	seedSrc := source()
	seedExp := experience(seedSrc, "coypu migration", 0.5)
	seedEnt := entity("coypu-project", seedSrc)

	w := trustWalk{
		eng:   &Engine{Store: st},
		scope: scope,
		seed:  []anamnesia.SearchHit{{Domain: anamnesia.DomainExperience, Experience: seedExp}},
		label: map[uuid.UUID]string{seedExp.ID: "seed"},
	}
	// Weakest edge first, and carrying the highest row trust.
	for _, n := range []struct {
		name      string
		edgeTrust float32
		rowTrust  float32
		into      *uuid.UUID
	}{
		{"lorikeet-project", 0.2, 0.9, &w.low},
		{"marmoset-project", 0.5, 0.7, &w.mid},
		{"nightjar-project", 0.9, 0.5, &w.high},
	} {
		src := source()
		exp := experience(src, n.name+" review", n.rowTrust)
		ent := entity(n.name, src)
		if err := st.CreateEdge(ctx, &anamnesia.Edge{
			From: seedEnt.ID, To: ent.ID, Kind: "related_to", Trust: n.edgeTrust,
		}); err != nil {
			t.Fatal(err)
		}
		*n.into = exp.ID
		w.label[exp.ID] = fmt.Sprintf("%s (edge %.1f, row %.1f)", n.name, n.edgeTrust, n.rowTrust)
	}
	return w
}

// show renders a hit list using the fixture's labels, so a failure names
// which edge each row came in on rather than printing bare UUIDs.
func (w trustWalk) show(ids []uuid.UUID) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		if l, ok := w.label[id]; ok {
			out[i] = l
			continue
		}
		out[i] = id.String()
	}
	return strings.Join(out, ", ")
}

func sameIDs(got, want []uuid.UUID) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestGraphCandidatesRankByTheTrustOfTheEdgeThatReachedThem: the graph
// channel's rank is scored into RRF exactly like a vector rank, so it
// has to mean something. It used to mean the row's own trust, which
// extraction sets to a near-constant 0.7 — a rank that was, on real
// data, whatever order the rows came back in. This asserts the order is
// the walk's own confidence: the neighbour reached over the 0.9 edge
// first, then 0.5, then 0.2, against a fixture whose row trusts say the
// opposite.
func TestGraphCandidatesRankByTheTrustOfTheEdgeThatReachedThem(t *testing.T) {
	w := newTrustWalk(t, "retrieval-graph-edge-trust")
	out, err := w.eng.graphExpand(context.Background(), Query{
		Scope:      w.scope,
		Domains:    []anamnesia.Domain{anamnesia.DomainFact, anamnesia.DomainExperience},
		GraphSeedN: 5, GraphFanout: 10, GraphK: 20,
	}, w.seed)
	if err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{w.high, w.mid, w.low}
	if got := hitIDs(out); !sameIDs(got, want) {
		t.Errorf("graph candidates ranked\n got: %s\nwant: %s\n(the rank feeding RRF is not the walk's confidence)",
			w.show(got), w.show(want))
	}
}

// TestGraphKKeepsTheRowsTheWalkIsMostSureOf: GraphK is a cut, not a
// sample. With three reachable rows and room for two, the two that
// survive must be the ones reached over the strongest edges — including
// through hitsForSources' own LIMIT, which does the first cut inside
// SQL and used to make it on the same near-constant row trust.
func TestGraphKKeepsTheRowsTheWalkIsMostSureOf(t *testing.T) {
	w := newTrustWalk(t, "retrieval-graph-truncate")
	out, err := w.eng.graphExpand(context.Background(), Query{
		Scope:      w.scope,
		Domains:    []anamnesia.Domain{anamnesia.DomainFact, anamnesia.DomainExperience},
		GraphSeedN: 5, GraphFanout: 10, GraphK: 2,
	}, w.seed)
	if err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{w.high, w.mid}
	if got := hitIDs(out); !sameIDs(got, want) {
		t.Errorf("GraphK=2 kept\n got: %s\nwant: %s\n(the cut is dropping rows the walk trusts more than the ones it keeps)",
			w.show(got), w.show(want))
	}
}
