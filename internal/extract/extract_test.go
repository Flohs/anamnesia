package extract

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia-open-source/internal/llm"
	"github.com/flohs/anamnesia-open-source/internal/store"
	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

// fakeLLM returns whatever the test pre-loads into Ops.
type fakeLLM struct {
	Ops    []Operation
	Calls  int
	Prompt string
	System string
	Schema json.RawMessage
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func (f *fakeLLM) Model() string                                        { return "fake" }
func (f *fakeLLM) Complete(context.Context, string) (string, error)     { return "", nil }
func (f *fakeLLM) Distill(context.Context, llm.DistillInput, any) error { return nil }
func (f *fakeLLM) Extract(_ context.Context, in llm.DistillInput, out any) error {
	f.Calls++
	f.Prompt = in.User
	f.System = in.System
	f.Schema = in.Schema
	rawOps := make([]json.RawMessage, len(f.Ops))
	for i, op := range f.Ops {
		b, err := json.Marshal(op)
		if err != nil {
			return err
		}
		rawOps[i] = b
	}
	raw, _ := json.Marshal(opsResponse{Operations: rawOps})
	return json.Unmarshal(raw, out)
}

// TestExecuteOps exercises the operation execution paths against a real
// Postgres. The test reads ANAMNESIA_TEST_DATABASE_URL; if absent it
// skips so the unit test suite stays green offline.
func TestExecuteOps(t *testing.T) {
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

	user := "extract-test-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, user)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	pid, err := st.EnsureProject(ctx, uid, "test-project")
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	scope := anamnesia.Scope{UserID: uid, ProjectID: &pid}

	src := &anamnesia.Source{
		Scope:      scope,
		Kind:       "chat-turn",
		Title:      "test",
		OccurredAt: time.Now().UTC(),
		RawContent: "By the way, I always run pnpm test --filter=foo before committing.",
	}
	if err := st.InsertSource(ctx, src); err != nil {
		t.Fatalf("insert source: %v", err)
	}

	fake := &fakeLLM{Ops: []Operation{
		{
			Op:        "ADD_FACT",
			FactScope: "project",
			Key:       "test_command",
			Value:     mustJSON(map[string]any{"cmd": "pnpm test --filter=foo"}),
			Trust:     0.9,
			Source:    "chat",
		},
		{
			Op:         "ADD_EXPERIENCE",
			Kind:       "strategy",
			Title:      "pre-commit test workflow",
			Body:       "User runs `pnpm test --filter=foo` before every commit on this project.",
			Importance: 0.7,
			Topic:      "testing",
		},
		{Op: "NOOP"},
	}}

	ex := &Extractor{
		Store: st,
		LLM:   fake,
	}
	n, err := ex.Run(ctx, src)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 executed ops, got %d", n)
	}
	if fake.Calls != 1 {
		t.Fatalf("expected 1 LLM call, got %d", fake.Calls)
	}
	// Prompt should contain the source content + now + candidates fields.
	for _, want := range []string{"pnpm test", "now", "occurred_at", "source_kind"} {
		if !strings.Contains(fake.Prompt, want) {
			t.Errorf("prompt missing %q\nprompt was: %s", want, fake.Prompt)
		}
	}

	// Verify the fact landed.
	facts, err := st.ListFacts(ctx, scope, anamnesia.FactScopeProject, 10)
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	if facts[0].Key != "test_command" {
		t.Errorf("fact key = %q, want test_command", facts[0].Key)
	}
	if facts[0].SourceID == nil || *facts[0].SourceID != src.ID {
		t.Errorf("fact.source_id mismatch (got %v want %v)", facts[0].SourceID, src.ID)
	}

	// Verify the experience landed.
	exps, err := st.ListExperiences(ctx, scope, 10)
	if err != nil {
		t.Fatalf("list experiences: %v", err)
	}
	if len(exps) != 1 {
		t.Fatalf("expected 1 experience, got %d", len(exps))
	}
	if exps[0].Topic != "testing" {
		t.Errorf("experience topic = %q, want testing", exps[0].Topic)
	}
	if exps[0].SourceID == nil || *exps[0].SourceID != src.ID {
		t.Errorf("experience.source_id mismatch")
	}
	if exps[0].OccurredAt == nil {
		t.Error("experience.occurred_at not set")
	}

	// Now exercise UPDATE_FACT + DELETE_FACT against the row we just created.
	factID := facts[0].ID
	src2 := &anamnesia.Source{
		Scope:      scope,
		Kind:       "chat-turn",
		OccurredAt: time.Now().UTC(),
		RawContent: "Actually I switched to bun test now.",
	}
	if err := st.InsertSource(ctx, src2); err != nil {
		t.Fatalf("insert source 2: %v", err)
	}
	fake.Ops = []Operation{
		{
			Op:    "UPDATE_FACT",
			ID:    factID.String(),
			Value: mustJSON(map[string]any{"cmd": "bun test"}),
			Trust: 0.95,
		},
	}
	n, err = ex.Run(ctx, src2)
	if err != nil {
		t.Fatalf("extract 2: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 op, got %d", n)
	}
	updated, err := st.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if cmd, _ := updated.Value["cmd"].(string); cmd != "bun test" {
		t.Errorf("UPDATE_FACT did not stick, got value=%v", updated.Value)
	}

	// DELETE_FACT
	src3 := &anamnesia.Source{
		Scope:      scope,
		Kind:       "chat-turn",
		OccurredAt: time.Now().UTC(),
		RawContent: "Forget about the test command preference.",
	}
	if err := st.InsertSource(ctx, src3); err != nil {
		t.Fatalf("insert source 3: %v", err)
	}
	fake.Ops = []Operation{{Op: "DELETE_FACT", ID: factID.String()}}
	if _, err := ex.Run(ctx, src3); err != nil {
		t.Fatalf("extract 3: %v", err)
	}
	deleted, err := st.GetFact(ctx, factID)
	if err != nil {
		t.Fatalf("get fact after delete: %v", err)
	}
	if deleted.DeletedAt == nil {
		t.Error("DELETE_FACT did not soft-delete")
	}
}

// TestSurpriseGate covers the gate logic in isolation.
func TestSurpriseGate(t *testing.T) {
	// Temporal markers bypass the gate entirely.
	if !hasTemporalMarker("I just switched to bun") {
		t.Error("temporal marker missed: 'just'")
	}
	if !hasTemporalMarker("Yesterday I moved the auth to clerk") {
		t.Error("temporal marker missed: 'yesterday'")
	}
	if hasTemporalMarker("the build is broken") {
		t.Error("false positive: no temporal marker should match 'the build is broken'")
	}
}

// TestCommitmentPromptGating verifies the ADD_COMMITMENT instructions and
// schema variant are sent only when Config.ExtractCommitments is true.
// DB-free: the fake LLM returns a single NOOP so executeOp (and the
// store) is never reached.
func TestCommitmentPromptGating(t *testing.T) {
	ctx := context.Background()
	src := &anamnesia.Source{
		Scope:      anamnesia.Scope{UserID: uuid.New()},
		Kind:       "chat-turn",
		OccurredAt: time.Now().UTC(),
		RawContent: "Some content long enough to clear the min-content gate.",
	}

	t.Run("enabled", func(t *testing.T) {
		fake := &fakeLLM{Ops: []Operation{{Op: "NOOP"}}}
		ex := &Extractor{Cfg: Config{ExtractCommitments: true}, LLM: fake}
		if _, err := ex.Run(ctx, src); err != nil {
			t.Fatalf("run: %v", err)
		}
		if !strings.Contains(fake.System, "ADD_COMMITMENT") {
			t.Errorf("system prompt should include ADD_COMMITMENT instructions when enabled")
		}
		if !strings.Contains(string(fake.Schema), "ADD_COMMITMENT") {
			t.Errorf("schema should permit ADD_COMMITMENT when enabled")
		}
	})

	t.Run("disabled", func(t *testing.T) {
		fake := &fakeLLM{Ops: []Operation{{Op: "NOOP"}}}
		ex := &Extractor{Cfg: Config{ExtractCommitments: false}, LLM: fake}
		if _, err := ex.Run(ctx, src); err != nil {
			t.Fatalf("run: %v", err)
		}
		if strings.Contains(fake.System, "ADD_COMMITMENT") {
			t.Errorf("system prompt must NOT mention ADD_COMMITMENT when disabled")
		}
		if strings.Contains(string(fake.Schema), "ADD_COMMITMENT") {
			t.Errorf("schema must NOT permit ADD_COMMITMENT when disabled")
		}
	})

	t.Run("disabled op is a no-op", func(t *testing.T) {
		// With the flag off, even a stray ADD_COMMITMENT op must not reach
		// the (nil) store. executeOp returns nil before touching it.
		ex := &Extractor{Cfg: Config{ExtractCommitments: false}}
		if err := ex.executeOp(ctx, src, Operation{Op: "ADD_COMMITMENT", Body: "x"}); err != nil {
			t.Errorf("disabled ADD_COMMITMENT should be a no-op, got %v", err)
		}
	})
}
