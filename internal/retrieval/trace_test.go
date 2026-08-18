package retrieval

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// TestSearchRecordsItsStages runs against a real Postgres, because every
// stage of a search is SQL. It reads ANAMNESIA_TEST_DATABASE_URL and
// skips when that is absent, matching internal/extract.
func TestSearchRecordsItsStages(t *testing.T) {
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

	handle := "retrieval-trace-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, handle)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	pid, err := st.EnsureProject(ctx, uid, "trace-project")
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	scope := anamnesia.Scope{UserID: uid, ProjectID: &pid}
	if err := st.RecordExperience(ctx, &anamnesia.Experience{
		Scope: scope, Kind: anamnesia.ExperienceCase,
		Title: "chose pnpm over npm",
		Body:  "The team standardised on pnpm for every repository.",
	}); err != nil {
		t.Fatalf("record experience: %v", err)
	}

	rec := activity.New(4)
	tr := rec.Begin("retrieve", handle, "trace-project")
	eng := &Engine{Store: st} // no embedder, no reranker: the common local setup

	hits, err := eng.Search(ctx, Query{Scope: scope, Text: "pnpm", K: 5, Trace: tr})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	tr.End("ok", "done")

	got, ok := rec.Trace(tr.ID)
	if !ok {
		t.Fatal("trace not recorded")
	}
	var names []string
	for _, s := range got.Steps {
		names = append(names, s.Name)
	}
	want := []string{"query", "vector", "lexical", "fuse", "rerank"}
	if len(names) != len(want) {
		t.Fatalf("steps = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("steps = %v, want %v", names, want)
		}
	}

	// Without an embedder the vector stage cannot run, and the trace has
	// to say so rather than look like a search that found nothing.
	if vector := got.Steps[1].Detail; vector["skipped"] != true || vector["reason"] == "" {
		t.Errorf("vector step = %v, want it to explain why it did not run", vector)
	}
	if lexical := got.Steps[2].Detail; lexical["hits"] == nil {
		t.Errorf("lexical step = %v, want the hits it found", lexical)
	}
	if len(hits) == 0 {
		t.Fatal("the experience written above should have matched lexically")
	}
	ranked, okRanked := got.Steps[3].Detail["ranked"].([]map[string]any)
	if !okRanked || len(ranked) == 0 {
		t.Fatalf("fuse step = %v, want the fused ranking", got.Steps[3].Detail)
	}
	if _, ok := ranked[0]["rrf_score"]; !ok {
		t.Errorf("fused entry = %v, want an rrf_score", ranked[0])
	}
	if applied := got.Steps[4].Detail["applied"]; applied != false {
		t.Errorf("rerank step applied = %v, want false with no reranker configured", applied)
	}
}

func TestSearchWithoutATraceIsUnchanged(t *testing.T) {
	// The engine is called from the extractor's gate and candidate fetch
	// too. Those must not record anything, or one ingest would evict the
	// ring with three traces nobody asked for.
	eng := &Engine{}
	if _, err := eng.Search(context.Background(), Query{Text: "anything"}); err == nil {
		t.Error("a search with no store should still fail the same way")
	}
}
