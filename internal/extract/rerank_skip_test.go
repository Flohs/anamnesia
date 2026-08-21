package extract

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/retrieval"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// countingReranker records reaching a rerank provider. The candidate
// fetch runs once per ingested source, so a rerank call here is paid on
// every single ingest.
type countingReranker struct{ calls int }

func (r *countingReranker) Rerank(_ context.Context, _ string, hits []anamnesia.SearchHit) ([]anamnesia.SearchHit, error) {
	r.calls++
	return hits, nil
}

// TestCandidateFetchDoesNotPayForRerank pins the extractor's side of the
// contract. Query.SkipRerank existing is not enough: the extractor has to
// set it, and this is the call site that makes it worth having.
func TestCandidateFetchDoesNotPayForRerank(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	uid, err := st.EnsureUser(ctx, "extract-rerank-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	scope := anamnesia.Scope{UserID: uid}
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "numbat-diet",
		Value: map[string]any{"note": "the numbat eats termites, numbat feeding notes"},
	}); err != nil {
		t.Fatalf("upsert fact: %v", err)
	}

	rr := &countingReranker{}
	ex := &Extractor{Retrieval: &retrieval.Engine{Store: st, Reranker: rr}}

	_, hits, err := ex.candidates(ctx, scope, "numbat", 10)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("candidate fetch returned nothing, so this proves nothing about rerank")
	}
	if rr.calls != 0 {
		t.Errorf("reranker called %d times during candidate fetch, want 0", rr.calls)
	}
}
