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

// TestMalformedSegmentSourceIDsDoNotBreakTheGraphPass: a graph source's
// metadata may be absent (an older checkpoint) or shaped unexpectedly (a
// source posted by something other than doCheckpoint). Either way runGraph
// must finish without error, not panic or fail the pass. Every case here
// keeps the LLM response to NOOP, so runGraph never reaches the store —
// the property under test is purely that reading the metadata is safe.
func TestMalformedSegmentSourceIDsDoNotBreakTheGraphPass(t *testing.T) {
	cases := map[string]map[string]any{
		"no metadata at all":       nil,
		"metadata without the key": {"other": "value"},
		"not a list":               {"segment_source_ids": "6f1c1f7a-0a1a-4a7a-9a7a-1a7a1a7a1a7a"},
		"list of non-strings":      {"segment_source_ids": []any{42, true}},
		"list of non-uuid strings": {"segment_source_ids": []any{"not-a-uuid"}},
	}
	for name, meta := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			fake := &fakeLLM{Ops: []Operation{{Op: "NOOP"}}}
			ex := &Extractor{Cfg: Config{ExtractGraph: true}, LLM: fake}
			src := &anamnesia.Source{
				Scope: anamnesia.Scope{UserID: uuid.New()}, Kind: graphSourceKind,
				OccurredAt: time.Now().UTC(),
				RawContent: "Some content long enough to clear the min-content gate.",
				Metadata:   meta,
			}
			if _, err := ex.Run(ctx, src); err != nil {
				t.Fatalf("run: %v", err)
			}
		})
	}
}

// TestSegmentSourceIDsFromMetadataParsesValidIDs is the companion to the
// tolerance test above: given well-formed metadata (the shape doCheckpoint
// actually sends, JSON round-tripped through the store so string entries
// arrive as []any), every id parses.
func TestSegmentSourceIDsFromMetadataParsesValidIDs(t *testing.T) {
	id1, id2 := uuid.New(), uuid.New()
	ex := &Extractor{}
	got := ex.segmentSourceIDsFromMetadata(map[string]any{
		"segment_source_ids": []any{id1.String(), id2.String()},
	})
	if len(got) != 2 || got[0] != id1 || got[1] != id2 {
		t.Errorf("segmentSourceIDsFromMetadata = %v, want [%s %s]", got, id1, id2)
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
