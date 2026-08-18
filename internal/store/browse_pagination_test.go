package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

func TestBrowseFactsPagesWithoutGapsOrRepeats(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	const total = 5
	for i := 0; i < total; i++ {
		if err := st.UpsertFact(ctx, &anamnesia.Fact{
			Scope: scope, FactKind: anamnesia.FactScopeProject,
			Key:   fmt.Sprintf("key.%d", i),
			Value: map[string]any{"v": i}, Trust: 0.7,
		}); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[uuid.UUID]int{}
	cursor := ""
	for page := 0; page < 10; page++ {
		facts, next, err := st.BrowseFacts(ctx, Browse{Scope: scope, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, f := range facts {
			seen[f.ID]++
		}
		if next == "" {
			break
		}
		if next == cursor {
			t.Fatal("the cursor did not advance")
		}
		cursor = next
	}
	if len(seen) != total {
		t.Errorf("saw %d distinct facts across the pages, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("fact %s appeared %d times", id, n)
		}
	}
}

func TestBrowseFactsFiltersBySubstring(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	for _, key := range []string{"deploy.target", "editor.theme"} {
		if err := st.UpsertFact(ctx, &anamnesia.Fact{
			Scope: scope, FactKind: anamnesia.FactScopeProject,
			Key: key, Value: map[string]any{"v": "x"}, Trust: 0.7,
		}); err != nil {
			t.Fatal(err)
		}
	}
	facts, _, err := st.BrowseFacts(ctx, Browse{Scope: scope, Q: "DEPLOY"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Key != "deploy.target" {
		t.Errorf("got %d facts, want just deploy.target (the match is case-insensitive)", len(facts))
	}
}

func TestBrowseExperiencesFiltersByAbstraction(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	for _, level := range []int{0, 0, 1} {
		if err := st.RecordExperience(ctx, &anamnesia.Experience{
			Scope: scope, Kind: anamnesia.ExperienceCase,
			Title: "t", Body: "b", Abstraction: level,
		}); err != nil {
			t.Fatal(err)
		}
	}
	one := 1
	exps, _, err := st.BrowseExperiences(ctx, Browse{Scope: scope, Abstraction: &one})
	if err != nil {
		t.Fatal(err)
	}
	if len(exps) != 1 || exps[0].Abstraction != 1 {
		t.Errorf("got %d experiences, want the single abstraction-1 row", len(exps))
	}
}

func TestBrowseSourcesFiltersByState(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	src := &anamnesia.Source{Scope: scope, Kind: "chat-turn", RawContent: "content"}
	if err := st.InsertSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	other := &anamnesia.Source{Scope: scope, Kind: "chat-turn", RawContent: "more content"}
	if err := st.InsertSource(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkSkipped(ctx, other.ID); err != nil {
		t.Fatal(err)
	}

	pending, _, err := st.BrowseSources(ctx, Browse{Scope: scope, State: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != src.ID {
		t.Errorf("pending = %d rows, want only the one still waiting", len(pending))
	}
	skipped, _, err := st.BrowseSources(ctx, Browse{Scope: scope, State: "skipped"})
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0].ID != other.ID {
		t.Errorf("skipped = %d rows, want the one the gate passed over", len(skipped))
	}
}

func TestBrowseEdgesIsScopedThroughItsEntities(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	from := &anamnesia.Entity{Scope: scope, Kind: "person", Name: "ada"}
	to := &anamnesia.Entity{Scope: scope, Kind: "project", Name: "anamnesia"}
	if err := st.UpsertEntity(ctx, from); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEntity(ctx, to); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateEdge(ctx, &anamnesia.Edge{
		From: from.ID, To: to.ID, Kind: "works_on",
	}); err != nil {
		t.Fatal(err)
	}

	edges, _, err := st.BrowseEdges(ctx, Browse{Scope: scope})
	if err != nil {
		t.Fatalf("browse edges: %v", err)
	}
	if len(edges) != 1 || edges[0].Kind != "works_on" {
		t.Fatalf("edges = %+v, want the one just created", edges)
	}

	// An edge belonging to someone else must not appear.
	otherStore, otherScope := testStore(t)
	elsewhere, _, err := otherStore.BrowseEdges(ctx, Browse{Scope: otherScope})
	if err != nil {
		t.Fatal(err)
	}
	if len(elsewhere) != 0 {
		t.Errorf("another user sees %d edges, want none", len(elsewhere))
	}
}

func TestBrowseLimitIsCapped(t *testing.T) {
	st, scope := testStore(t)
	if _, _, err := st.BrowseFacts(context.Background(), Browse{Scope: scope, Limit: 100000}); err != nil {
		t.Fatalf("an absurd limit should be capped, not rejected: %v", err)
	}
}
