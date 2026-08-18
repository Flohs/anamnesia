package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/retrieval"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// The retrieve path runs on every prompt, so its trace is the one an
// observer sees most. It needs a real database: scope resolution and
// both halves of the search are SQL.
func TestRetrieveRecordsATrace(t *testing.T) {
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
	handle := "retrieve-trace-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := st.EnsureProject(ctx, uid, "trace-project")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordExperience(ctx, &anamnesia.Experience{
		Scope: anamnesia.Scope{UserID: uid, ProjectID: &pid},
		Kind:  anamnesia.ExperienceCase,
		Title: "chose pnpm over npm",
		Body:  "The team standardised on pnpm.",
	}); err != nil {
		t.Fatal(err)
	}

	rec := activity.New(4)
	srv := testServer(t, Deps{
		Store:          st,
		Retrieval:      &retrieval.Engine{Store: st},
		Activity:       rec,
		DefaultUser:    handle,
		DefaultProject: "trace-project",
	})

	body, _ := json.Marshal(map[string]any{"prompt": "pnpm"})
	resp, err := http.Post(srv.URL+"/v1/retrieve", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	snap := rec.Snapshot()
	if len(snap.Traces) != 1 {
		t.Fatalf("traces = %d, want exactly one for one prompt", len(snap.Traces))
	}
	tr, ok := rec.Trace(snap.Traces[0].ID)
	if !ok {
		t.Fatal("trace not fetchable")
	}
	if tr.Kind != "retrieve" || tr.Status != "ok" {
		t.Errorf("trace = %s/%s, want retrieve/ok", tr.Kind, tr.Status)
	}
	if tr.User != handle || tr.Project != "trace-project" {
		t.Errorf("trace scope = %s/%s, want %s/trace-project", tr.User, tr.Project, handle)
	}
	var names []string
	for _, s := range tr.Steps {
		names = append(names, s.Name)
	}
	want := []string{"query", "vector", "lexical", "fuse", "rerank", "result"}
	if len(names) != len(want) {
		t.Fatalf("steps = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("steps = %v, want %v", names, want)
		}
	}
	result := tr.Steps[len(tr.Steps)-1].Detail
	if result["hits"] == nil {
		t.Errorf("result step = %v, want what was handed back to the caller", result)
	}
}

func TestSessionStartRecordsWhatItLoaded(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	rec := activity.New(4)
	st, scope, _, _, base := dbServer(t, rec)
	if err := st.RecordExperience(context.Background(), &anamnesia.Experience{
		Scope: scope, Kind: anamnesia.ExperienceCase, Title: "a memory", Body: "body",
	}); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{})
	resp, err := http.Post(base+"/v1/sessions/start", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	snap := rec.Snapshot()
	if len(snap.Traces) != 1 {
		t.Fatalf("traces = %d, want one", len(snap.Traces))
	}
	tr, _ := rec.Trace(snap.Traces[0].ID)
	if tr.Kind != "session-start" || tr.Status != "ok" {
		t.Errorf("trace = %s/%s, want session-start/ok", tr.Kind, tr.Status)
	}
	if len(tr.Steps) != 1 || tr.Steps[0].Name != "load" {
		t.Fatalf("steps = %+v, want one load step", tr.Steps)
	}
	detail := tr.Steps[0].Detail
	if detail["experiences"] != 1 {
		t.Errorf("load detail = %v, want the one experience counted", detail)
	}
}

func TestIngestRecordsItsArrival(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	rec := activity.New(4)
	_, _, _, _, base := dbServer(t, rec)

	body, _ := json.Marshal(map[string]any{
		"kind": "chat-turn", "content": "a checkpoint that will be extracted later",
	})
	resp, err := http.Post(base+"/v1/ingest", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var ingested map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&ingested); err != nil {
		t.Fatal(err)
	}

	snap := rec.Snapshot()
	if len(snap.Traces) != 1 {
		t.Fatalf("traces = %d, want the arrival recorded", len(snap.Traces))
	}
	tr, _ := rec.Trace(snap.Traces[0].ID)
	if tr.Kind != "queued" {
		t.Errorf("kind = %q, want queued: the extractor opens the ingest trace later", tr.Kind)
	}
	if len(tr.Steps) != 1 || tr.Steps[0].Name != "queued" {
		t.Fatalf("steps = %+v", tr.Steps)
	}
	// The source id is what joins this to the extractor's own trace.
	if tr.Steps[0].Detail["source_id"] != ingested["source_id"] {
		t.Errorf("source_id = %v, want %v", tr.Steps[0].Detail["source_id"], ingested["source_id"])
	}
}
