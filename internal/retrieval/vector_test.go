package retrieval

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// conceptEmbedder projects text onto one dimension per concept word, so
// cosine distance is a known function of which concepts a text mentions.
// That makes vector ORDER assertable, which okEmbedder's constant vector
// cannot do: with every row equidistant, any ordering looks correct.
//
// The final dimension is a constant bias so no vector is all-zero, which
// pgvector's cosine operator has no meaningful answer for.
type conceptEmbedder struct {
	dims     int
	concepts []string
}

func (c conceptEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, c.dims)
		low := strings.ToLower(t)
		for j, w := range c.concepts {
			if strings.Contains(low, w) {
				v[j] = 1
			}
		}
		v[c.dims-1] = 1
		out[i] = v
	}
	return out, nil
}
func (c conceptEmbedder) Dims() int     { return c.dims }
func (c conceptEmbedder) Model() string { return "concept-test-embedder" }

func vectorFixture(t *testing.T) (*store.Store, *Engine, anamnesia.Scope, conceptEmbedder) {
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
	uid, err := st.EnsureUser(ctx, "vector-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	emb := conceptEmbedder{dims: 1536, concepts: []string{"quokka", "wombat", "numbat"}}
	return st, &Engine{Store: st, Embedder: emb}, anamnesia.Scope{UserID: uid}, emb
}

// factWithVector writes a fact whose embedding is the projection of its
// own text, the way the embed worker eventually would.
func factWithVector(t *testing.T, st *store.Store, emb conceptEmbedder, scope anamnesia.Scope, key, body string) uuid.UUID {
	t.Helper()
	v, err := emb.Embed(context.Background(), []string{key + " " + body})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	f := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: key,
		Value: map[string]any{"v": body}, Embedding: v[0], EmbedModel: emb.Model(),
	}
	if err := st.UpsertFact(context.Background(), f); err != nil {
		t.Fatalf("upsert %s: %v", key, err)
	}
	return f.ID
}

func TestVectorFactsRanksTheNearestEmbeddingFirst(t *testing.T) {
	st, eng, scope, emb := vectorFixture(t)
	ctx := context.Background()
	want := factWithVector(t, st, emb, scope, "a.quokka", "the quokka naps")
	factWithVector(t, st, emb, scope, "b.wombat", "the wombat digs")
	factWithVector(t, st, emb, scope, "c.numbat", "the numbat forages")

	qv, _ := emb.Embed(ctx, []string{"quokka"})
	hits, err := eng.vectorFacts(ctx, scope, qv[0], 10)
	if err != nil {
		t.Fatalf("vectorFacts: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("vector search returned nothing")
	}
	if hits[0].Fact == nil || hits[0].Fact.ID != want {
		t.Errorf("nearest hit = %v, want the quokka fact: vector order is not by embedding distance", hitIDs(hits))
	}
}

func TestVectorFactsSkipsRowsWithNoEmbedding(t *testing.T) {
	// An unembedded row has no position in the space. Returning it would
	// mean ordering by whatever the index does with NULL.
	st, eng, scope, emb := vectorFixture(t)
	ctx := context.Background()
	unembedded := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "d.quokka",
		Value: map[string]any{"v": "quokka quokka quokka"},
	}
	if err := st.UpsertFact(ctx, unembedded); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	factWithVector(t, st, emb, scope, "a.quokka", "the quokka naps")

	qv, _ := emb.Embed(ctx, []string{"quokka"})
	hits, err := eng.vectorFacts(ctx, scope, qv[0], 10)
	if err != nil {
		t.Fatalf("vectorFacts: %v", err)
	}
	for _, h := range hits {
		if h.Fact != nil && h.Fact.ID == unembedded.ID {
			t.Error("vector search returned a fact with no embedding")
		}
	}
}

func TestVectorFactsStaysInsideItsScope(t *testing.T) {
	st, eng, scope, emb := vectorFixture(t)
	ctx := context.Background()
	other, err := st.EnsureUser(ctx, "vector-other-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	otherScope := anamnesia.Scope{UserID: other}
	stranger := factWithVector(t, st, emb, otherScope, "a.quokka", "the quokka naps")
	factWithVector(t, st, emb, scope, "b.wombat", "the wombat digs")

	qv, _ := emb.Embed(ctx, []string{"quokka"})
	hits, err := eng.vectorFacts(ctx, scope, qv[0], 10)
	if err != nil {
		t.Fatalf("vectorFacts: %v", err)
	}
	for _, h := range hits {
		if h.Fact != nil && h.Fact.ID == stranger {
			t.Fatal("vector search crossed into another user's memory")
		}
	}
}

func TestVectorFactsHonoursItsLimit(t *testing.T) {
	st, eng, scope, emb := vectorFixture(t)
	ctx := context.Background()
	for _, c := range []string{"quokka", "wombat", "numbat"} {
		factWithVector(t, st, emb, scope, c+".fact", "the "+c+" exists")
	}
	qv, _ := emb.Embed(ctx, []string{"quokka"})
	hits, err := eng.vectorFacts(ctx, scope, qv[0], 2)
	if err != nil {
		t.Fatalf("vectorFacts: %v", err)
	}
	if len(hits) > 2 {
		t.Errorf("got %d hits for k=2: the limit is what bounds the candidate set fusion sees", len(hits))
	}
}

// TestSearchStampsVectorRank is the fusion-level counterpart: until now
// every test in this package ran with a nil qvec, so nothing ever
// asserted that the vector channel reaches Search's ranking at all.
func TestSearchStampsVectorRank(t *testing.T) {
	st, eng, scope, emb := vectorFixture(t)
	ctx := context.Background()
	factWithVector(t, st, emb, scope, "a.quokka", "the quokka naps")

	hits, err := eng.Search(ctx, Query{Scope: scope, Text: "quokka"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("search returned nothing")
	}
	var sawVector bool
	for _, h := range hits {
		if h.VectorRank > 0 {
			sawVector = true
		}
	}
	if !sawVector {
		t.Error("no hit carries a vector_rank: the vector channel never reached fusion")
	}
}

func TestVectorExperiencesOnlyRawExcludesSummaries(t *testing.T) {
	// The benchmark and citation flows set OnlyRaw to keep consolidator
	// summaries out. That filter had no test on the vector path.
	st, eng, scope, emb := vectorFixture(t)
	ctx := context.Background()
	for _, e := range []struct {
		title string
		abs   int
	}{{"raw quokka sighting", 0}, {"quokka summary", 1}} {
		v, _ := emb.Embed(ctx, []string{e.title})
		exp := &anamnesia.Experience{
			Scope: scope, Kind: anamnesia.ExperienceCase, Title: e.title,
			Body: e.title, Abstraction: e.abs, Embedding: v[0], EmbedModel: emb.Model(),
		}
		if err := st.RecordExperience(ctx, exp); err != nil {
			t.Fatalf("record %q: %v", e.title, err)
		}
	}
	qv, _ := emb.Embed(ctx, []string{"quokka"})
	hits, err := eng.vectorExperiences(ctx, scope, qv[0], 10, true)
	if err != nil {
		t.Fatalf("vectorExperiences: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no raw experience returned")
	}
	for _, h := range hits {
		if h.Experience != nil && h.Experience.Abstraction != 0 {
			t.Errorf("OnlyRaw returned an abstraction=%d experience", h.Experience.Abstraction)
		}
	}
}
