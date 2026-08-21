package store

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// embeddingIsNull reads the column GetFact deliberately does not select.
func embeddingIsNull(t *testing.T, st *Store, id uuid.UUID) bool {
	t.Helper()
	var isNull bool
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT embedding IS NULL FROM facts WHERE id = $1`, id).Scan(&isNull); err != nil {
		t.Fatalf("read embedding: %v", err)
	}
	return isNull
}

func vec(dims int) []float32 {
	v := make([]float32, dims)
	v[0] = 1
	return v
}

// TestChangingAValueClearsItsStaleEmbedding is a retrieval correctness
// bug, not a tidiness one. The extractor never supplies an embedding, and
// the embed worker only backfills rows WHERE embedding IS NULL, so an
// upsert that kept the old vector left a fact whose text says one thing
// and whose vector says another — findable by vector search only under
// wording it no longer has, forever, because nothing re-embeds it.
func TestChangingAValueClearsItsStaleEmbedding(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	f := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.commute",
		Value: map[string]any{"v": "cycles to work"}, Embedding: vec(1536),
		EmbedModel: "test-model",
	}
	if err := st.UpsertFact(ctx, f); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if embeddingIsNull(t, st, f.ID) {
		t.Fatal("fixture is wrong: the first write should have stored an embedding")
	}

	// The extractor's path: a new value, no embedding supplied.
	changed := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.commute",
		Value: map[string]any{"v": "takes the tram to work"},
	}
	if err := st.UpsertFact(ctx, changed); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if !embeddingIsNull(t, st, f.ID) {
		t.Error("the value changed but the old embedding survived: the row is now findable only by wording it no longer has, and the backfill worker only looks for NULL")
	}
}

// TestReassertingAValueKeepsItsEmbedding is the other half. Re-asserting
// an unchanged value must not throw away a good vector, or every repeated
// mention would queue a needless re-embed.
func TestReassertingAValueKeepsItsEmbedding(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	f := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.commute",
		Value: map[string]any{"v": "cycles to work"}, Embedding: vec(1536),
		EmbedModel: "test-model",
	}
	if err := st.UpsertFact(ctx, f); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	same := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.commute",
		Value: map[string]any{"v": "cycles to work"},
	}
	if err := st.UpsertFact(ctx, same); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if embeddingIsNull(t, st, f.ID) {
		t.Error("re-asserting the same value discarded a valid embedding")
	}
}

// TestAValueChangeThatBringsItsOwnEmbeddingKeepsIt: a caller that does
// supply a vector for the new value must not have it discarded.
func TestAValueChangeThatBringsItsOwnEmbeddingKeepsIt(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	f := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.commute",
		Value: map[string]any{"v": "cycles to work"}, Embedding: vec(1536),
		EmbedModel: "old-model",
	}
	if err := st.UpsertFact(ctx, f); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	changed := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.commute",
		Value: map[string]any{"v": "takes the tram to work"}, Embedding: vec(1536),
		EmbedModel: "new-model",
	}
	if err := st.UpsertFact(ctx, changed); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if embeddingIsNull(t, st, f.ID) {
		t.Error("an embedding supplied with the new value was discarded")
	}
	var model string
	if err := st.Pool.QueryRow(ctx, `SELECT embed_model FROM facts WHERE id = $1`, f.ID).Scan(&model); err != nil {
		t.Fatalf("read embed_model: %v", err)
	}
	if model != "new-model" {
		t.Errorf("embed_model = %q, want the model that produced the stored vector", model)
	}
}

// TestAClearedEmbeddingAlsoClearsItsModelLabel: leaving embed_model set
// on a NULL embedding claims a vector was produced by a model when there
// is no vector at all.
func TestAClearedEmbeddingAlsoClearsItsModelLabel(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	f := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.commute",
		Value: map[string]any{"v": "cycles to work"}, Embedding: vec(1536),
		EmbedModel: "test-model",
	}
	if err := st.UpsertFact(ctx, f); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	changed := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.commute",
		Value: map[string]any{"v": "takes the tram to work"},
	}
	if err := st.UpsertFact(ctx, changed); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	var model *string
	if err := st.Pool.QueryRow(ctx, `SELECT embed_model FROM facts WHERE id = $1`, f.ID).Scan(&model); err != nil {
		t.Fatalf("read embed_model: %v", err)
	}
	if model != nil && *model != "" {
		t.Errorf("embed_model = %q with a NULL embedding: it names a model for a vector that does not exist", *model)
	}
}
