package extract

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// fakeEmbedder returns a fixed vector per input string, so a test can
// control what NearestEntities would see without a real embedding
// provider or a database. A lookup miss is an error, not a zero vector —
// a test that forgets to register a name should fail loudly rather than
// silently embed everything at the origin.
type fakeEmbedder struct {
	Vecs map[string][]float32
	Err  error
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := f.Vecs[t]
		if !ok {
			return nil, errors.New("fakeEmbedder: no vector registered for " + t)
		}
		out[i] = v
	}
	return out, nil
}
func (f *fakeEmbedder) Dims() int     { return 0 }
func (f *fakeEmbedder) Model() string { return "fake" }

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

// The entityCandidatesWith tests below exercise candidate recall —
// the generous distance filter and the embedder/lookup-failure fallback
// — without a database: the store's NearestEntities is passed in as a
// plain function value instead of going through *store.Store, which
// requires a live Postgres for every call. There is no kind filter to
// test here on purpose: candidates are not filtered by kind at recall
// time (the future op's kind isn't known yet), so kind-safety lives
// downstream — in the prompt (graphSystemPrompt tells the model a
// shared name under a different kind is a different thing) and in the
// schema (the (scope, kind, name) unique index makes a same-name,
// wrong-kind upsert create a new row rather than merge into the
// candidate's). entityCandidates (the Extractor method) is the thin
// wrapper that binds NearestEntities to entityCandidatesWith; it is
// exercised end to end by TestEntityCandidatesOfferExistingEntitiesFromEarlierSessions
// in graph_db_test.go.

func TestEntityCandidatesWithKeepsMatchesWithinDistance(t *testing.T) {
	ctx := context.Background()
	scope := anamnesia.Scope{UserID: uuid.New()}
	existing := &anamnesia.Entity{ID: uuid.New(), Kind: "person", Name: "priya-raman"}
	emb := &fakeEmbedder{Vecs: map[string][]float32{"the checkpoint text": {0.1, 0.2}}}
	nearest := func(context.Context, anamnesia.Scope, []float32, int) ([]store.EntityMatch, error) {
		return []store.EntityMatch{{Entity: existing, Distance: 0.1}}, nil
	}

	got := entityCandidatesWith(ctx, emb, nearest, 0.45, scope, "the checkpoint text", graphCandidateK, nil)
	if len(got) != 1 || got[0].Entity.ID != existing.ID {
		t.Fatalf("entityCandidatesWith = %v, want the one match within distance 0.45", got)
	}
}

func TestEntityCandidatesWithDropsMatchesBeyondDistance(t *testing.T) {
	ctx := context.Background()
	scope := anamnesia.Scope{UserID: uuid.New()}
	emb := &fakeEmbedder{Vecs: map[string][]float32{"the checkpoint text": {0.9, 0.9}}}
	nearest := func(context.Context, anamnesia.Scope, []float32, int) ([]store.EntityMatch, error) {
		return []store.EntityMatch{{
			Entity:   &anamnesia.Entity{ID: uuid.New(), Kind: "project", Name: "sku-catalog"},
			Distance: 0.5,
		}}, nil
	}

	got := entityCandidatesWith(ctx, emb, nearest, 0.45, scope, "the checkpoint text", graphCandidateK, nil)
	if len(got) != 0 {
		t.Errorf("entityCandidatesWith = %v, want none (distance 0.5 is beyond 0.45)", got)
	}
}

func TestEntityCandidatesWithEmbedderErrorYieldsNoCandidates(t *testing.T) {
	ctx := context.Background()
	scope := anamnesia.Scope{UserID: uuid.New()}
	emb := &fakeEmbedder{Err: errors.New("embedding provider is down")}
	nearestCalled := false
	nearest := func(context.Context, anamnesia.Scope, []float32, int) ([]store.EntityMatch, error) {
		nearestCalled = true
		return nil, nil
	}

	got := entityCandidatesWith(ctx, emb, nearest, 0.45, scope, "the checkpoint text", graphCandidateK, nil)
	if got != nil {
		t.Errorf("entityCandidatesWith = %v, want nil: an embedder failure must not fail the pass, it must yield no candidates", got)
	}
	if nearestCalled {
		t.Error("NearestEntities was called after the embedder failed; it must never run without a vector")
	}
}

func TestEntityCandidatesWithLookupErrorYieldsNoCandidates(t *testing.T) {
	ctx := context.Background()
	scope := anamnesia.Scope{UserID: uuid.New()}
	emb := &fakeEmbedder{Vecs: map[string][]float32{"the checkpoint text": {0.1, 0.2}}}
	nearest := func(context.Context, anamnesia.Scope, []float32, int) ([]store.EntityMatch, error) {
		return nil, errors.New("store is down")
	}

	got := entityCandidatesWith(ctx, emb, nearest, 0.45, scope, "the checkpoint text", graphCandidateK, nil)
	if got != nil {
		t.Errorf("entityCandidatesWith = %v, want nil: a lookup failure must not fail the pass", got)
	}
}

func TestEntityCandidateListShapesForThePrompt(t *testing.T) {
	id := uuid.New()
	matches := []store.EntityMatch{{Entity: &anamnesia.Entity{ID: id, Kind: "person", Name: "priya-raman"}, Distance: 0.1}}
	got := entityCandidateList(matches)
	if len(got) != 1 {
		t.Fatalf("entityCandidateList = %v, want 1 candidate", got)
	}
	c := got[0]
	if c.Domain != "entity" || c.ID != id.String() || c.Body != "priya-raman" {
		t.Errorf("candidate = %+v, want domain=entity id=%s body=priya-raman", c, id)
	}
	if kind, _ := c.Meta["kind"].(string); kind != "person" {
		t.Errorf("candidate.Meta[kind] = %v, want %q", c.Meta["kind"], "person")
	}
}
