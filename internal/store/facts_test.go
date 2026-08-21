package store

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// factProvenanceFixture writes one fact asserted by source A, and returns a
// second source B that a conflicting upsert can claim to originate from.
func factProvenanceFixture(t *testing.T) (st *Store, scope anamnesia.Scope, factID, srcAID, srcBID uuid.UUID) {
	t.Helper()
	st, scope = testStore(t)
	ctx := context.Background()

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
	return st, scope, f.ID, srcA.ID, srcB.ID
}

// TestUpsertFactIdenticalValueLeavesProvenanceWithOriginalSource is the
// ADD_FACT counterpart of the UPDATE_FACT bug fixed in
// internal/extract/provenance_test.go: an upsert that merely re-asserts the
// value already stored must not hand authorship to the new writer.
// source_id is read as "where this content came from" by retrieval eval
// labels and by internal/extract's hitSourceID-style lookups, so a source
// that repeats a fact without changing it must not claim it.
func TestUpsertFactIdenticalValueLeavesProvenanceWithOriginalSource(t *testing.T) {
	st, scope, factID, srcAID, srcBID := factProvenanceFixture(t)
	ctx := context.Background()

	// Source B re-asserts the exact value already stored under source A.
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.bike_type",
		Value: map[string]any{"v": "hybrid bike"}, SourceID: &srcBID,
	}); err != nil {
		t.Fatalf("upsert fact: %v", err)
	}

	got, err := st.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if got.SourceID == nil || *got.SourceID != srcAID {
		t.Errorf("source_id = %v, want source A (%v): re-asserting a value is not authoring it",
			got.SourceID, srcAID)
	}
}

// TestUpsertFactChangedValueMovesProvenanceToNewSource is the other half:
// when the incoming value genuinely differs, the upserting source is the
// correct new owner. Without this, the fix above could regress to "never
// move provenance on upsert", which is just as wrong in the other direction.
func TestUpsertFactChangedValueMovesProvenanceToNewSource(t *testing.T) {
	st, scope, factID, _, srcBID := factProvenanceFixture(t)
	ctx := context.Background()

	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.bike_type",
		Value: map[string]any{"v": "road bike"}, SourceID: &srcBID,
	}); err != nil {
		t.Fatalf("upsert fact: %v", err)
	}

	// Since migration 0010 a changed value becomes a new row and the old
	// one is superseded, so the assertion is about the *current* row.
	// factID now names the superseded version, which correctly still
	// holds the old value and the source that authored it.
	var gotSrc uuid.UUID
	var gotVal string
	if err := st.Pool.QueryRow(ctx, `
		SELECT source_id, value->>'v' FROM facts
		WHERE user_id = $1 AND key = 'user.bike_type'
		  AND deleted_at IS NULL AND superseded_by IS NULL`,
		scope.UserID).Scan(&gotSrc, &gotVal); err != nil {
		t.Fatalf("read current row: %v", err)
	}
	if gotSrc != srcBID {
		t.Errorf("current source_id = %v, want source B (%v): the new value came from B", gotSrc, srcBID)
	}
	if gotVal != "road bike" {
		t.Errorf("current value = %q, want the updated value", gotVal)
	}
	old, err := st.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("get superseded fact: %v", err)
	}
	if v, _ := old.Value["v"].(string); v != "hybrid bike" {
		t.Errorf("superseded value = %q, want the value it held", v)
	}
}

// TestUpsertFactFirstInsertSetsProvenanceNormally covers the non-conflict
// path: a brand-new fact must still get its source_id from the insert, not
// just retain whatever the CASE expression happens to fall through to.
func TestUpsertFactFirstInsertSetsProvenanceNormally(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	src := &anamnesia.Source{Scope: scope, Kind: "chat-turn", RawContent: "I prefer pnpm over npm."}
	if err := st.InsertSource(ctx, src); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	f := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.pkg_manager",
		Value: map[string]any{"v": "pnpm"}, SourceID: &src.ID,
	}
	if err := st.UpsertFact(ctx, f); err != nil {
		t.Fatalf("upsert fact: %v", err)
	}

	got, err := st.GetFact(ctx, f.ID)
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if got.SourceID == nil || *got.SourceID != src.ID {
		t.Errorf("source_id = %v, want %v: a first insert must set provenance", got.SourceID, src.ID)
	}
}

// TestUpsertFactWithoutIncomingSourceNeverClearsExisting pins the existing
// COALESCE behaviour: a conflicting upsert that carries no source_id at all
// (e.g. a manual facts_upsert MCP call) must not blank out the fact's
// provenance, whether or not the value it carries has changed.
func TestUpsertFactWithoutIncomingSourceNeverClearsExisting(t *testing.T) {
	st, scope, factID, srcAID, _ := factProvenanceFixture(t)
	ctx := context.Background()

	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.bike_type",
		Value: map[string]any{"v": "road bike"},
	}); err != nil {
		t.Fatalf("upsert fact: %v", err)
	}

	got, err := st.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if got.SourceID == nil || *got.SourceID != srcAID {
		t.Errorf("source_id = %v, want it left as %v: a NULL incoming source_id must never clear provenance",
			got.SourceID, srcAID)
	}
}
