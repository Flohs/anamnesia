package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/extract"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// A source the model read and found nothing in must not look like a source
// the model never read. Both execute zero operations, and marking both
// `skipped` meant the database could not say how often the model is paid to
// say nothing: the only honest answer was an estimate off a weekly trend.
//
// The short source stands in for every path that short-circuits before the
// model, the surprise gate included; they all reach the worker as the same
// ModelCalled=false and take the same branch.
func TestExtractionStateSeparatesAGateSkipFromAModelThatFoundNothing(t *testing.T) {
	st, scope := newEmbedTestStore(t, "extract-state")
	ctx := context.Background()

	source := func(content string) *anamnesia.Source {
		src := &anamnesia.Source{
			ID: uuid.New(), Scope: scope, Kind: "chat-turn",
			Title: "a checkpoint", OccurredAt: time.Now().UTC(), RawContent: content,
		}
		if err := st.InsertSource(ctx, src); err != nil {
			t.Fatalf("insert source: %v", err)
		}
		return src
	}

	neverRead := source("too short")
	readAndEmpty := source("A checkpoint long enough to reach the model, which finds nothing in it.")

	w := &Worker{
		Cfg:      Config{ExtractBatch: 64, ExtractConcurrency: 1, Extract: extract.Config{}},
		Store:    st,
		Embedder: fixedEmbedder{},
		LLM:      &fakeBriefingLLM{out: `{"operations":[{"op":"NOOP"}]}`},
		Log:      discardLog(),
	}
	if _, err := w.tickExtract(ctx); err != nil {
		t.Fatalf("extract tick: %v", err)
	}

	state := func(src *anamnesia.Source) (string, int) {
		t.Helper()
		got, err := st.GetSource(ctx, src.ID)
		if err != nil {
			t.Fatalf("get source: %v", err)
		}
		return got.ExtractionState, got.OpsProduced
	}

	if gotState, gotOps := state(neverRead); gotState != "skipped" || gotOps != 0 {
		t.Errorf("a source the model never read = %q/%d ops, want skipped/0", gotState, gotOps)
	}
	if gotState, gotOps := state(readAndEmpty); gotState != "done" || gotOps != 0 {
		t.Errorf("a source the model read and found nothing in = %q/%d ops, want done/0", gotState, gotOps)
	}
}
