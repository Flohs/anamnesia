package retrieval

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// recordingReranker counts how often the rerank stage actually reached a
// provider. Rerank is a paid network call, so "was it invoked" is the
// behaviour under test, not the ordering it would have produced.
type recordingReranker struct{ calls int }

func (r *recordingReranker) Rerank(_ context.Context, _ string, hits []anamnesia.SearchHit) ([]anamnesia.SearchHit, error) {
	r.calls++
	return hits, nil
}

// rerankScope builds a scope holding one searchable fact.
func rerankScope(t *testing.T) (*store.Store, anamnesia.Scope) {
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
	uid, err := st.EnsureUser(ctx, "rerank-skip-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	scope := anamnesia.Scope{UserID: uid}
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "wombat-habitat",
		Value: map[string]any{"note": "the wombat burrow survey covers three wombat sites"},
	}); err != nil {
		t.Fatalf("upsert fact: %v", err)
	}
	return st, scope
}

func TestSkipRerankKeepsTheRerankerOutOfTheLoop(t *testing.T) {
	// The extractor searches once per source purely to assemble merge
	// candidates for a prompt. Nothing reads that order, so paying a
	// rerank call per ingested source buys nothing.
	st, scope := rerankScope(t)
	rr := &recordingReranker{}
	eng := &Engine{Store: st, Reranker: rr}

	hits, err := eng.Search(context.Background(), Query{Scope: scope, Text: "wombat", SkipRerank: true})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if rr.calls != 0 {
		t.Errorf("reranker called %d times, want 0 when SkipRerank is set", rr.calls)
	}
	if len(hits) == 0 {
		t.Error("skipping rerank must not also skip retrieval")
	}
}

func TestRerankStillRunsWhenNotSkipped(t *testing.T) {
	// Guards the obvious way to break the above: making the skip
	// unconditional and silently disabling rerank for /v1/retrieve too.
	st, scope := rerankScope(t)
	rr := &recordingReranker{}
	eng := &Engine{Store: st, Reranker: rr}

	if _, err := eng.Search(context.Background(), Query{Scope: scope, Text: "wombat"}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if rr.calls != 1 {
		t.Errorf("reranker called %d times, want 1 for an ordinary search", rr.calls)
	}
}
