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
	handle := "consolidate-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, handle)
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

	// A ring large enough that this scope's trace is not evicted.
	// ConsolidationRun is server-wide and now opens one trace per scope,
	// and the test database is shared with every other package, so a pass
	// covers far more scopes than this test creates.
	rec := activity.New(4096)
	lm := &fakeDistiller{}
	if err := ConsolidationRun(ctx, st, lm, ConsolidateConfig{}, discardLog(), 7*24*time.Hour, rec); err != nil {
		t.Fatalf("consolidation: %v", err)
	}
	if lm.calls == 0 {
		t.Fatal("the distiller was never called, so there is nothing to trace")
	}

	// The pass opens with a survey of the scopes it covers.
	snap := rec.Snapshot()
	var sawScopes bool
	for _, head := range snap.Traces {
		tr, ok := rec.Trace(head.ID)
		if !ok || len(tr.Steps) == 0 {
			continue
		}
		if tr.Steps[0].Name == "scopes" {
			if scopes, ok := tr.Steps[0].Detail["scopes"].([]map[string]any); ok && len(scopes) > 0 {
				sawScopes = true
			}
		}
	}
	if !sawScopes {
		t.Error("no trace surveyed the scopes the pass covered")
	}

	// This scope's own trace carries the reasoning: what clustered, what
	// the model made of it, and what was written. Found by its content
	// rather than by position, because a server-wide pass traces every
	// scope in the shared database, not only this one.
	// Found by this test's own user handle, which is unique per run. The
	// fake distiller returns one fixed title for every call, so searching
	// by result would match whichever scope in the shared database
	// happened to be consolidated first.
	var tr *activity.Trace
	for _, head := range snap.Traces {
		if head.User != handle {
			continue
		}
		if cand, ok := rec.Trace(head.ID); ok {
			tr = cand
			break
		}
	}
	if tr == nil {
		t.Fatal("this scope got no trace of its own, so nothing recorded why it folded what it did")
	}
	if tr.Kind != "consolidate" {
		t.Errorf("trace kind = %q, want consolidate", tr.Kind)
	}
	if tr.Project != "consolidate-project" {
		t.Errorf("trace project = %q, want the scope it consolidated: a trace that names no project cannot be filtered to one", tr.Project)
	}
	distil := -1
	for i, st := range tr.Steps {
		if st.Name == "distil" {
			distil = i
			break
		}
	}
	if distil < 0 {
		t.Fatalf("no distil step in this scope's trace: %v", stepNames(tr.Steps))
	}
	if tr.Steps[distil].Detail["result_title"] != "the team standardised on pnpm" {
		t.Errorf("distil step = %v, want the model's result", tr.Steps[distil].Detail)
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
