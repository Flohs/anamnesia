package extract

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// provenanceFixture writes one fact asserted by source A, and returns a
// second source B that never mentioned it.
func provenanceFixture(t *testing.T) (*store.Store, anamnesia.Scope, *anamnesia.Source, *anamnesia.Source, uuid.UUID) {
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
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	uid, err := st.EnsureUser(ctx, "provenance-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	scope := anamnesia.Scope{UserID: uid}

	srcA := &anamnesia.Source{Scope: scope, Kind: "chat-turn", RawContent: "I ride a hybrid bike to work."}
	if err := st.InsertSource(ctx, srcA); err != nil {
		t.Fatalf("insert source A: %v", err)
	}
	srcB := &anamnesia.Source{Scope: scope, Kind: "chat-turn", RawContent: "I went to see a play at the community theater."}
	if err := st.InsertSource(ctx, srcB); err != nil {
		t.Fatalf("insert source B: %v", err)
	}

	f := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.bike_type",
		Value: map[string]any{"v": "hybrid bike"}, SourceID: &srcA.ID,
	}
	if err := st.UpsertFact(ctx, f); err != nil {
		t.Fatalf("upsert fact: %v", err)
	}
	return st, scope, srcA, srcB, f.ID
}

// TestANoOpUpdateDoesNotStealProvenance is the bug found on LongMemEval
// question 58bf7951: a session that never mentioned bikes owned
// user.bike_type, because UPDATE_FACT rewrote source_id whether or not it
// changed anything. Source-granularity retrieval labels read source_id as
// "where this came from", so a fact that changes hands without changing
// content corrupts them in both directions.
func TestANoOpUpdateDoesNotStealProvenance(t *testing.T) {
	st, _, srcA, srcB, factID := provenanceFixture(t)
	ctx := context.Background()
	ex := &Extractor{Store: st}

	// An UPDATE_FACT carrying no value: nothing about the fact changes.
	if _, err := ex.updateFact(ctx, srcB, Operation{Op: "UPDATE_FACT", ID: factID.String()}); err != nil {
		t.Fatalf("updateFact: %v", err)
	}

	got, err := st.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if got.SourceID == nil || *got.SourceID != srcA.ID {
		t.Errorf("source_id = %v, want source A (%v): an update that changed nothing must not claim authorship",
			got.SourceID, srcA.ID)
	}
	if v, _ := got.Value["v"].(string); v != "hybrid bike" {
		t.Errorf("value = %q, want it untouched", v)
	}
}

// TestAnUpdateThatChangesTheValueTakesProvenance is the other half: when
// the content really does come from the updating source, that source is
// the correct owner. Without this, the fix above could be "never move
// provenance", which would be just as wrong in the opposite direction.
func TestAnUpdateThatChangesTheValueTakesProvenance(t *testing.T) {
	st, _, _, srcB, factID := provenanceFixture(t)
	ctx := context.Background()
	ex := &Extractor{Store: st}

	op := Operation{Op: "UPDATE_FACT", ID: factID.String(), Value: []byte(`{"v":"road bike"}`)}
	if _, err := ex.updateFact(ctx, srcB, op); err != nil {
		t.Fatalf("updateFact: %v", err)
	}

	got, err := st.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if got.SourceID == nil || *got.SourceID != srcB.ID {
		t.Errorf("source_id = %v, want source B (%v): the new value came from B",
			got.SourceID, srcB.ID)
	}
	if v, _ := got.Value["v"].(string); v != "road bike" {
		t.Errorf("value = %q, want the updated value", v)
	}
}

// TestAnUpdateToAnIdenticalValueDoesNotStealProvenance closes the gap the
// first test leaves: the model commonly echoes a candidate back verbatim
// as an UPDATE_FACT. That carries a value, so a len(op.Value)>0 check
// alone would still hand over provenance for no change at all.
func TestAnUpdateToAnIdenticalValueDoesNotStealProvenance(t *testing.T) {
	st, _, srcA, srcB, factID := provenanceFixture(t)
	ctx := context.Background()
	ex := &Extractor{Store: st}

	op := Operation{Op: "UPDATE_FACT", ID: factID.String(), Value: []byte(`{"v":"hybrid bike"}`)}
	if _, err := ex.updateFact(ctx, srcB, op); err != nil {
		t.Fatalf("updateFact: %v", err)
	}

	got, err := st.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if got.SourceID == nil || *got.SourceID != srcA.ID {
		t.Errorf("source_id = %v, want source A (%v): re-asserting a value is not authoring it",
			got.SourceID, srcA.ID)
	}
}
