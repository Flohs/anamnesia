package retrieval

import (
	"context"
	"testing"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// The fused score cannot say whether anything actually matched: RRF is
// rank-based, so the best of an irrelevant pool scores what the best of a
// relevant one scores. The cosine distance the vector channel already
// ordered by is the only absolute number available, so it has to survive
// into the hit rather than being thrown away.

func TestVectorFactsCarryTheDistanceTheyWereRankedBy(t *testing.T) {
	st, eng, scope, emb := vectorFixture(t)
	ctx := context.Background()
	factWithVector(t, st, emb, scope, "a.quokka", "the quokka naps")
	factWithVector(t, st, emb, scope, "b.wombat", "the wombat digs")

	qv, _ := emb.Embed(ctx, []string{"quokka"})
	hits, err := eng.vectorFacts(ctx, scope, qv[0], 10, false)
	if err != nil {
		t.Fatalf("vectorFacts: %v", err)
	}
	if len(hits) < 2 {
		t.Fatalf("want both facts back, got %d", len(hits))
	}
	for i, h := range hits {
		if h.Distance == nil {
			t.Fatalf("hit %d carries no distance", i)
		}
	}
	if *hits[0].Distance >= *hits[1].Distance {
		t.Errorf("distances do not follow the ranking: %v then %v",
			*hits[0].Distance, *hits[1].Distance)
	}
}

func TestVectorExperiencesCarryTheDistanceTheyWereRankedBy(t *testing.T) {
	st, eng, scope, emb := vectorFixture(t)
	ctx := context.Background()
	for _, title := range []string{"the quokka naps", "the wombat digs"} {
		v, _ := emb.Embed(ctx, []string{title})
		exp := &anamnesia.Experience{
			Scope: scope, Kind: anamnesia.ExperienceCase, Title: title,
			Body: title, Embedding: v[0], EmbedModel: emb.Model(),
		}
		if err := st.RecordExperience(ctx, exp); err != nil {
			t.Fatalf("record %q: %v", title, err)
		}
	}

	qv, _ := emb.Embed(ctx, []string{"quokka"})
	hits, err := eng.vectorExperiences(ctx, scope, qv[0], 10, false)
	if err != nil {
		t.Fatalf("vectorExperiences: %v", err)
	}
	if len(hits) < 2 {
		t.Fatalf("want both experiences back, got %d", len(hits))
	}
	for i, h := range hits {
		if h.Distance == nil {
			t.Fatalf("hit %d carries no distance", i)
		}
	}
	if *hits[0].Distance >= *hits[1].Distance {
		t.Errorf("distances do not follow the ranking: %v then %v",
			*hits[0].Distance, *hits[1].Distance)
	}
}

// A lexical hit has no position in the vector space, so it must not
// arrive carrying a number that would be read as one. Nil is the
// difference between "this matched badly" and "there is nothing to judge
// this by".
func TestLexicalFactsCarryNoDistance(t *testing.T) {
	st, eng, scope, emb := vectorFixture(t)
	ctx := context.Background()
	factWithVector(t, st, emb, scope, "a.quokka", "the quokka naps")

	hits, err := eng.lexicalFacts(ctx, scope, "quokka", 10, false)
	if err != nil {
		t.Fatalf("lexicalFacts: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("lexical search returned nothing")
	}
	for i, h := range hits {
		if h.Distance != nil {
			t.Errorf("lexical hit %d carries distance %v", i, *h.Distance)
		}
	}
}

// A perfect match is at distance zero, which a plain float64 cannot tell
// apart from a channel that never set one.
func TestAnExactMatchIsDistanceZeroAndNotAbsent(t *testing.T) {
	st, eng, scope, emb := vectorFixture(t)
	ctx := context.Background()
	factWithVector(t, st, emb, scope, "a.quokka", "quokka")

	qv, _ := emb.Embed(ctx, []string{"quokka"})
	hits, err := eng.vectorFacts(ctx, scope, qv[0], 10, false)
	if err != nil {
		t.Fatalf("vectorFacts: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("vector search returned nothing")
	}
	if hits[0].Distance == nil {
		t.Fatal("an exact match arrived with no distance at all")
	}
	if *hits[0].Distance > 1e-6 {
		t.Errorf("identical vectors should be distance 0, got %v", *hits[0].Distance)
	}
}
