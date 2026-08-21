package retrieval

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// historyFixture writes one key twice, so it has a current value and a
// superseded one, both embedded and both lexically findable.
func historyFixture(t *testing.T) (*Engine, anamnesia.Scope) {
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
	uid, err := st.EnsureUser(ctx, "history-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	scope := anamnesia.Scope{UserID: uid}
	for _, v := range []string{"commutes by quokka ferry", "commutes by wombat tram"} {
		if err := st.UpsertFact(ctx, &anamnesia.Fact{
			Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.commute",
			Value: map[string]any{"v": v},
		}); err != nil {
			t.Fatalf("upsert %q: %v", v, err)
		}
	}
	return &Engine{Store: st}, scope
}

func values(hits []anamnesia.SearchHit) []string {
	var out []string
	for _, h := range hits {
		if h.Fact != nil {
			v, _ := h.Fact.Value["v"].(string)
			out = append(out, v)
		}
	}
	return out
}

// TestHistoryIsInvisibleByDefault is the property the whole design rests
// on: the hooks inject memory into every prompt, and an agent shown both
// "quokka ferry" and "wombat tram" has to work out which is true.
func TestHistoryIsInvisibleByDefault(t *testing.T) {
	eng, scope := historyFixture(t)

	hits, err := eng.Search(context.Background(), Query{Scope: scope, Text: "commutes"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	got := values(hits)
	for _, v := range got {
		if v == "commutes by quokka ferry" {
			t.Errorf("a superseded value reached an ordinary search: %v", got)
		}
	}
	if len(got) == 0 {
		t.Fatal("the current value should still be found")
	}
}

// TestIncludeHistoryReturnsTheOldValueToo: with the flag, the old value
// IS the answer ("what did I use to do").
func TestIncludeHistoryReturnsTheOldValueToo(t *testing.T) {
	eng, scope := historyFixture(t)

	hits, err := eng.Search(context.Background(), Query{
		Scope: scope, Text: "commutes", IncludeHistory: true,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	got := values(hits)
	var sawOld, sawCurrent bool
	for _, v := range got {
		switch v {
		case "commutes by quokka ferry":
			sawOld = true
		case "commutes by wombat tram":
			sawCurrent = true
		}
	}
	if !sawOld {
		t.Errorf("IncludeHistory did not return the superseded value: %v", got)
	}
	if !sawCurrent {
		t.Errorf("IncludeHistory dropped the current value: %v", got)
	}
}

// TestAHistoricalHitCarriesItsDates: a caller has to be able to label an
// old value as old, and say when it stopped being true, without a second
// query. SearchHit gains no fields for this; Fact already has them.
func TestAHistoricalHitCarriesItsDates(t *testing.T) {
	eng, scope := historyFixture(t)

	hits, err := eng.Search(context.Background(), Query{
		Scope: scope, Text: "commutes", IncludeHistory: true,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, h := range hits {
		if h.Fact == nil {
			continue
		}
		if v, _ := h.Fact.Value["v"].(string); v != "commutes by quokka ferry" {
			continue
		}
		if h.Fact.SupersededBy == nil {
			t.Error("the historical hit has no superseded_by: nothing marks it as superseded")
		}
		if h.Fact.ValidTo == nil {
			t.Error("the historical hit has no valid_to: nothing says when it stopped being true")
		}
		return
	}
	t.Fatal("the superseded value was not returned, so its dates could not be checked")
}
