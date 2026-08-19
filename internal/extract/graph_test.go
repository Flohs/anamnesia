package extract

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

func TestGraphSourceIsIgnoredWhenTheFlagIsOff(t *testing.T) {
	ctx := context.Background()
	fake := &fakeLLM{Ops: []Operation{{Op: "NOOP"}}}
	ex := &Extractor{Cfg: Config{}, LLM: fake}
	src := &anamnesia.Source{
		Scope: anamnesia.Scope{UserID: uuid.New()}, Kind: graphSourceKind,
		OccurredAt: time.Now().UTC(),
		RawContent: "Some content long enough to clear the min-content gate.",
	}
	n, err := ex.Run(ctx, src)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 0 {
		t.Errorf("wrote %d operations with graph.extract off, want 0", n)
	}
	if fake.Calls != 0 {
		t.Errorf("made %d model calls with graph.extract off, want 0", fake.Calls)
	}
}

func TestAnOrdinarySourceNeverRunsTheGraphPass(t *testing.T) {
	// The flag being on must not change what a normal checkpoint does.
	ctx := context.Background()
	fake := &fakeLLM{Ops: []Operation{{Op: "NOOP"}}}
	ex := &Extractor{Cfg: Config{ExtractGraph: true}, LLM: fake}
	src := &anamnesia.Source{
		Scope: anamnesia.Scope{UserID: uuid.New()}, Kind: "chat-turn",
		OccurredAt: time.Now().UTC(),
		RawContent: "Some content long enough to clear the min-content gate.",
	}
	if _, err := ex.Run(ctx, src); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(fake.System, "ADD_ENTITY") {
		t.Error("an ordinary source was sent the graph prompt")
	}
}

func TestGraphPromptIsSentForAGraphSource(t *testing.T) {
	ctx := context.Background()
	fake := &fakeLLM{}
	ex := &Extractor{Cfg: Config{ExtractGraph: true}, LLM: fake}
	src := &anamnesia.Source{
		Scope: anamnesia.Scope{UserID: uuid.New()}, Kind: graphSourceKind,
		OccurredAt: time.Now().UTC(),
		RawContent: "Rotterdam writes local time; every other site writes UTC.",
	}
	if _, err := ex.Run(ctx, src); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Calls != 1 {
		t.Fatalf("made %d model calls, want exactly 1", fake.Calls)
	}
	for _, want := range []string{"ADD_ENTITY", "ADD_EDGE"} {
		if !strings.Contains(fake.System, want) {
			t.Errorf("graph prompt does not mention %s", want)
		}
	}
	if strings.Contains(fake.System, "ADD_FACT") {
		t.Error("the graph prompt offers ADD_FACT; it must not")
	}
}

func TestEdgeWithAnUnresolvableEndpointIsDropped(t *testing.T) {
	// The model names entities, not uuids. An edge naming something that
	// was never created and does not exist in scope cannot be written, and
	// dropping it silently would make the graph quietly wrong.
	ops := []graphOperation{
		{Op: "ADD_ENTITY", Kind: "site", Name: "rotterdam"},
		{Op: "ADD_EDGE", From: "rotterdam", To: "a-place-never-mentioned", Kind: "reports_to"},
	}
	resolved, dropped := resolveEdges(ops, map[string]uuid.UUID{"rotterdam": uuid.New()})
	if len(resolved) != 0 {
		t.Errorf("resolved %d edges, want 0", len(resolved))
	}
	if len(dropped) != 1 || !strings.Contains(dropped[0], "a-place-never-mentioned") {
		t.Errorf("dropped = %v, want one entry naming the missing endpoint", dropped)
	}
}

func TestEntityNamesAreNormalisedBeforeUpsert(t *testing.T) {
	// entities_identity is unique on (scope, kind, name) but dedupes on the
	// LITERAL name, so normalisation has to happen before the store call or
	// three spellings become three nodes the database is happy with.
	for _, in := range []string{"The Rotterdam Warehouse", "Rotterdam warehouse", "rotterdam  warehouse"} {
		if got := normaliseEntityName(in); got != "rotterdam-warehouse" {
			t.Errorf("normaliseEntityName(%q) = %q, want %q", in, got, "rotterdam-warehouse")
		}
	}
}
