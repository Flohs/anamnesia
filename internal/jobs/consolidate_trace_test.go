package jobs

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/llm"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// fakeDistiller returns one fixed insight, so the test is about the
// trace rather than about a model.
type fakeDistiller struct{ calls int }

func (f *fakeDistiller) Model() string                                    { return "fake-distiller" }
func (f *fakeDistiller) Complete(context.Context, string) (string, error) { return "", nil }
func (f *fakeDistiller) Extract(context.Context, llm.DistillInput, any) error {
	return nil
}
func (f *fakeDistiller) Distill(_ context.Context, _ llm.DistillInput, out any) error {
	f.calls++
	raw, _ := json.Marshal(map[string]any{
		"title": "the team standardised on pnpm", "body": "two sessions agreed on it",
		"outcome": "success", "importance": 0.7, "kind": "strategy",
	})
	return json.Unmarshal(raw, out)
}

func TestConsolidationRecordsItsReasoning(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	uid, err := st.EnsureUser(ctx, "consolidate-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatal(err)
	}
	pid, err := st.EnsureProject(ctx, uid, "consolidate-project")
	if err != nil {
		t.Fatal(err)
	}
	scope := anamnesia.Scope{UserID: uid, ProjectID: &pid}

	// Two experiences pointing the same way, so they cluster.
	vec := make([]float32, 1536)
	for i := range vec {
		vec[i] = float32(i%3) + 0.5
	}
	for _, title := range []string{"used pnpm here", "used pnpm there"} {
		exp := &anamnesia.Experience{
			Scope: scope, Kind: anamnesia.ExperienceCase, Title: title, Body: "body",
		}
		if err := st.RecordExperience(ctx, exp); err != nil {
			t.Fatal(err)
		}
		if err := st.SetExperienceEmbedding(ctx, exp.ID, vec, "test"); err != nil {
			t.Fatal(err)
		}
	}

	rec := activity.New(4)
	lm := &fakeDistiller{}
	if err := ConsolidationRun(ctx, st, lm, ConsolidateConfig{}, discardLog(), 7*24*time.Hour, rec); err != nil {
		t.Fatalf("consolidation: %v", err)
	}
	if lm.calls == 0 {
		t.Fatal("the distiller was never called, so there is nothing to trace")
	}

	snap := rec.Snapshot()
	if len(snap.Traces) != 1 {
		t.Fatalf("traces = %d, want one per pass", len(snap.Traces))
	}
	tr, _ := rec.Trace(snap.Traces[0].ID)
	if tr.Kind != "consolidate" || tr.Status != "ok" {
		t.Errorf("trace = %s/%s, want consolidate/ok", tr.Kind, tr.Status)
	}
	// A pass is server-wide, so it may also fold other scopes that
	// happen to be in this database. The shape being asserted is one
	// scope's worth of it: the pass opens with scopes, and every
	// distillation is a cluster, then a distil, then a write.
	if tr.Steps[0].Name != "scopes" {
		t.Fatalf("first step = %q, want scopes", tr.Steps[0].Name)
	}
	if scopes, ok := tr.Steps[0].Detail["scopes"].([]map[string]any); !ok || len(scopes) == 0 {
		t.Errorf("scopes step = %v, want the scopes it covered", tr.Steps[0].Detail)
	}
	distil := -1
	for i, s := range tr.Steps {
		if s.Name == "distil" && s.Detail["result_title"] == "the team standardised on pnpm" {
			distil = i
			break
		}
	}
	if distil < 0 {
		t.Fatalf("no distil step carried the model's result: %v", stepNames(tr.Steps))
	}
	if tr.Steps[distil].Detail["model"] != "fake-distiller" {
		t.Errorf("distil step = %v, want the model named", tr.Steps[distil].Detail)
	}
	if distil+1 >= len(tr.Steps) || tr.Steps[distil+1].Name != "write" {
		t.Fatalf("steps after distil = %v, want a write", stepNames(tr.Steps[distil:]))
	}
	written, ok := tr.Steps[distil+1].Detail["written"].([]map[string]any)
	if !ok || len(written) != 1 || written[0]["abstraction"] != 1 {
		t.Errorf("write step = %v, want the abstraction-1 row it added", tr.Steps[distil+1].Detail)
	}
	cluster := -1
	for i := distil; i >= 0; i-- {
		if tr.Steps[i].Name == "cluster" {
			cluster = i
			break
		}
	}
	if cluster < 0 {
		t.Fatalf("no cluster step before the distillation: %v", stepNames(tr.Steps))
	}
	clusters, ok := tr.Steps[cluster].Detail["clusters"].([]map[string]any)
	if !ok || len(clusters) == 0 {
		t.Fatalf("cluster step = %v, want the clusters it formed", tr.Steps[cluster].Detail)
	}
	if _, ok := clusters[0]["centroid_similarity"]; !ok {
		t.Errorf("cluster = %v, want the similarity that held it together", clusters[0])
	}
}

func stepNames(steps []activity.Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}
