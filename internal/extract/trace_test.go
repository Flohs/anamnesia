package extract

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

func testSource(content string) *anamnesia.Source {
	return &anamnesia.Source{
		ID:         uuid.New(),
		Scope:      anamnesia.Scope{UserID: uuid.New()},
		Kind:       "chat-turn",
		Title:      "a checkpoint",
		OccurredAt: time.Now().UTC(),
		RawContent: content,
	}
}

func stepNames(steps []activity.Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

func onlyTrace(t *testing.T, rec *activity.Recorder) *activity.Trace {
	t.Helper()
	snap := rec.Snapshot()
	if len(snap.Traces) != 1 {
		t.Fatalf("traces = %d, want exactly one", len(snap.Traces))
	}
	tr, ok := rec.Trace(snap.Traces[0].ID)
	if !ok {
		t.Fatal("the trace just recorded is not fetchable")
	}
	return tr
}

func TestIngestTraceRecordsEachStage(t *testing.T) {
	rec := activity.New(4)
	fake := &fakeLLM{Ops: []Operation{{Op: "NOOP"}}}
	ex := &Extractor{LLM: fake, Activity: rec}
	src := testSource("Some content long enough to clear the min-content gate.")

	if _, err := ex.Run(context.Background(), src); err != nil {
		t.Fatalf("run: %v", err)
	}

	tr := onlyTrace(t, rec)
	if tr.Kind != "ingest" {
		t.Errorf("kind = %q, want ingest", tr.Kind)
	}
	if tr.Status != "skipped" {
		t.Errorf("status = %q, want skipped: the model returned NOOP and nothing was written", tr.Status)
	}
	got := stepNames(tr.Steps)
	want := []string{"source", "gate", "similar", "llm", "ops"}
	if len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("steps = %v, want %v", got, want)
		}
	}

	source := tr.Steps[0].Detail
	if source["source_id"] != src.ID.String() || source["kind"] != "chat-turn" {
		t.Errorf("source step = %v, want the source identified", source)
	}
	if source["bytes"] != len(src.RawContent) {
		t.Errorf("source step bytes = %v, want %d", source["bytes"], len(src.RawContent))
	}
	if gate := tr.Steps[1].Detail; gate["verdict"] != "keep" || gate["reason"] == "" {
		t.Errorf("gate step = %v, want a verdict with a reason behind it", gate)
	}
	model := tr.Steps[3].Detail
	if model["model"] != "fake" {
		t.Errorf("llm step = %v, want the model named", model)
	}
	if model["raw_response"] == nil {
		t.Error("llm step has no raw_response: what the model actually said is the point of this step")
	}
	if model["prompt_chars"] == nil || model["latency_ms"] == nil {
		t.Errorf("llm step = %v, want prompt size and latency", model)
	}
}

func TestShortSourceIsSkippedBeforeTheModelIsCalled(t *testing.T) {
	rec := activity.New(4)
	fake := &fakeLLM{Ops: []Operation{{Op: "NOOP"}}}
	ex := &Extractor{LLM: fake, Activity: rec}

	if _, err := ex.Run(context.Background(), testSource("too short")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Calls != 0 {
		t.Errorf("the model was called %d times for content below the minimum", fake.Calls)
	}

	tr := onlyTrace(t, rec)
	if tr.Status != "skipped" {
		t.Errorf("status = %q, want skipped", tr.Status)
	}
	if got := stepNames(tr.Steps); len(got) != 1 || got[0] != "source" {
		t.Errorf("steps = %v, want the trace to stop at source", got)
	}
	if tr.Summary == "" {
		t.Error("a skipped trace still has to say why")
	}
}

func TestExtractionRunsWithoutARecorder(t *testing.T) {
	fake := &fakeLLM{Ops: []Operation{{Op: "NOOP"}}}
	ex := &Extractor{LLM: fake}
	if _, err := ex.Run(context.Background(), testSource("Some content long enough to clear the gate.")); err != nil {
		t.Fatalf("run without a recorder: %v", err)
	}
}
