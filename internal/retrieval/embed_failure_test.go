package retrieval

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// failingEmbedder stands in for a configured embedder whose provider is
// unreachable, out of credit, or rate-limited.
type failingEmbedder struct{ err error }

func (f failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, f.err
}
func (f failingEmbedder) Dims() int     { return 1536 }
func (f failingEmbedder) Model() string { return "failing-test-embedder" }

// okEmbedder returns a fixed vector, so a search can reach the vector
// channel without a provider.
type okEmbedder struct{ dims int }

func (o okEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, o.dims)
		out[i][0] = 1
	}
	return out, nil
}
func (o okEmbedder) Dims() int     { return o.dims }
func (o okEmbedder) Model() string { return "ok-test-embedder" }

func embedScope(t *testing.T) (*store.Store, anamnesia.Scope) {
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
	uid, err := st.EnsureUser(ctx, "embed-fail-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	scope := anamnesia.Scope{UserID: uid}
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "pangolin.diet",
		Value: map[string]any{"v": "the pangolin eats ants"},
	}); err != nil {
		t.Fatalf("upsert fact: %v", err)
	}
	return st, scope
}

// TestAFailedQueryEmbeddingIsAnError is the bug an OpenRouter credit
// outage exposed: /v1/retrieve answered 200 with an empty hit list for a
// user holding 253 fully-embedded facts, because embedding the *query*
// failed and Search carried on without the vector channel. An empty
// result and a broken retrieval must not be indistinguishable — that is
// the same reasoning as "/v1/health must be able to fail".
func TestAFailedQueryEmbeddingIsAnError(t *testing.T) {
	st, scope := embedScope(t)
	eng := &Engine{Store: st, Embedder: failingEmbedder{err: errors.New("status 402: insufficient credits")}}

	_, err := eng.Search(context.Background(), Query{Scope: scope, Text: "what does the pangolin eat"})
	if err == nil {
		t.Fatal("search succeeded with a broken embedder: a caller cannot tell this from a genuine miss")
	}
	if !strings.Contains(err.Error(), "insufficient credits") {
		t.Errorf("error = %v, want the provider's reason preserved so the cause is diagnosable", err)
	}
}

// TestNoEmbedderConfiguredIsNotAnError keeps the fix from swallowing the
// legitimate local setup. An install with no embedder runs lexical-only
// by design, and that is a configuration, not a fault.
func TestNoEmbedderConfiguredIsNotAnError(t *testing.T) {
	st, scope := embedScope(t)
	eng := &Engine{Store: st} // no embedder

	hits, err := eng.Search(context.Background(), Query{Scope: scope, Text: "pangolin"})
	if err != nil {
		t.Fatalf("search with no embedder configured must still work: %v", err)
	}
	if len(hits) == 0 {
		t.Error("lexical search should still have found the fact")
	}
}

// TestAWorkingEmbedderStillSearches guards the obvious over-correction:
// returning an error whenever an embedder is present.
func TestAWorkingEmbedderStillSearches(t *testing.T) {
	st, scope := embedScope(t)
	eng := &Engine{Store: st, Embedder: okEmbedder{dims: 1536}}

	if _, err := eng.Search(context.Background(), Query{Scope: scope, Text: "pangolin"}); err != nil {
		t.Fatalf("search with a working embedder: %v", err)
	}
}

// TestAnEmptyQueryIsNotAnEmbedFailure: Search skips embedding when there
// is no text, which must not be reported as a broken embedder.
func TestAnEmptyQueryIsNotAnEmbedFailure(t *testing.T) {
	st, scope := embedScope(t)
	eng := &Engine{Store: st, Embedder: failingEmbedder{err: errors.New("should not be called")}}

	if _, err := eng.Search(context.Background(), Query{Scope: scope, Text: "   "}); err != nil {
		t.Fatalf("an empty query must not reach the embedder: %v", err)
	}
}
