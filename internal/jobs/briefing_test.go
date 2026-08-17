package jobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/llm"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// fakeBriefingLLM captures the prompt passed to Distill so we can
// assert the briefer sent the right shape. Returns out as the model's
// reply.
type fakeBriefingLLM struct {
	gotUser string
	out     string
}

func (f *fakeBriefingLLM) Model() string                                        { return "fake" }
func (f *fakeBriefingLLM) Complete(_ context.Context, _ string) (string, error) { return f.out, nil }
func (f *fakeBriefingLLM) Distill(_ context.Context, in llm.DistillInput, out any) error {
	f.gotUser = in.User
	return jsonRoundTrip([]byte(f.out), out)
}
func (f *fakeBriefingLLM) Extract(_ context.Context, in llm.DistillInput, out any) error {
	f.gotUser = in.User
	return jsonRoundTrip([]byte(f.out), out)
}

func TestBriefingPrompt_IncludesWindowAndExperiences(t *testing.T) {
	llmFake := &fakeBriefingLLM{out: `{"summary":"week summary","highlights":[],"adjacent":[]}`}
	b := Briefer{LLM: llmFake}
	since := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)
	t1 := since.Add(1 * time.Hour)
	t2 := since.Add(-3 * 24 * time.Hour)
	exps := []*anamnesia.Experience{
		{ID: uuid.New(), Title: "shipped extractor", Body: "wired pgvector", OccurredAt: &t1},
	}
	adj := []*anamnesia.Experience{
		{ID: uuid.New(), Title: "rrf debug notes", Body: "fused ranks", OccurredAt: &t2},
	}
	out, err := b.Brief(context.Background(), anamnesia.BriefingRequest{
		Since: since, Until: until, MaxAdjacent: 3,
	}, exps, adj)
	if err != nil {
		t.Fatalf("brief: %v", err)
	}
	if out.Summary != "week summary" {
		t.Fatalf("summary should reflect llm output, got %q", out.Summary)
	}
	if !strings.Contains(llmFake.gotUser, "2026-05-20") || !strings.Contains(llmFake.gotUser, "2026-05-27") {
		t.Fatalf("prompt missing window dates:\n%s", llmFake.gotUser)
	}
	if !strings.Contains(llmFake.gotUser, "shipped extractor") {
		t.Fatalf("prompt missing in-window title:\n%s", llmFake.gotUser)
	}
	if !strings.Contains(llmFake.gotUser, "rrf debug notes") {
		t.Fatalf("prompt missing adjacent title:\n%s", llmFake.gotUser)
	}
}

func TestBriefingStubWhenNoLLM(t *testing.T) {
	b := Briefer{LLM: nil}
	out, err := b.Brief(context.Background(), anamnesia.BriefingRequest{
		Since: time.Now().UTC().Add(-7 * 24 * time.Hour),
	}, nil, nil)
	if err != nil {
		t.Fatalf("brief: %v", err)
	}
	if !strings.Contains(out.Summary, "stub") {
		t.Fatalf("nil LLM should produce stub summary, got %q", out.Summary)
	}
}
