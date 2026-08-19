package extract

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

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
