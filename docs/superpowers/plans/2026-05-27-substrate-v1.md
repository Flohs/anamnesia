# Anamnesia Substrate v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add 6 new kernel primitives + 1 reference agent so non-Claude-Code agents can "dock onto" Anamnesia and act as a personal AI.

**Architecture:** Each primitive is a thin HTTP+MCP surface over existing Postgres tables; only the Commitments domain adds new storage. The reference agent lives in `examples/agents/cli-companion/` as a separate Go module that consumes only the public MCP surface — no `internal/` imports.

**Tech Stack:** Go 1.22+, pgx, `mark3labs/mcp-go`, goose migrations, Anthropic Agent SDK (Go).

**Spec:** `docs/superpowers/specs/2026-05-27-personal-ai-substrate-design.md`.

**Testing convention used in this codebase:** unit tests with fakes for parseable/renderable logic; HTTP/MCP/SQL verified by compiling and curling against a running stack (`./bin/anamnesia up && ./bin/anamnesia doctor`). Do **not** introduce a DB test harness — the project doesn't have one. Where a step says "write a test", the test must work against in-process stubs, not Postgres.

---

## File Structure

### Created
- `pkg/anamnesia/identity.go` — `Identity` type
- `pkg/anamnesia/briefing.go` — `BriefingRequest`, `BriefingResponse`
- `pkg/anamnesia/people.go` — `PersonView`
- `pkg/anamnesia/commitments.go` — `Commitment`, `CommitmentStatus`
- `internal/store/identity.go` — `GetIdentity(scope) (Identity, error)`
- `internal/store/identity_render.go` — pure render of `Identity` → markdown
- `internal/store/identity_render_test.go` — TDD for the render
- `internal/store/people.go` — `ListPeople(scope, limit) ([]PersonView, error)`
- `internal/store/capabilities.go` — `ListCapabilities(scope, limit) ([]Skill, error)` (freshness sort)
- `internal/store/commitments.go` — `RecordCommitment`, `ListCommitments`, `ResolveCommitment`
- `internal/store/migrations/0007_commitments.sql`
- `internal/jobs/briefing.go` — `Briefer.Brief(ctx, scope, req) (BriefingResponse, error)`
- `internal/jobs/briefing_test.go` — TDD for prompt assembly with fake LLM
- `examples/agents/cli-companion/go.mod`
- `examples/agents/cli-companion/main.go`
- `examples/agents/cli-companion/README.md`

### Modified
- `internal/store/experiences.go` — add `ListExperiencesInWindow`
- `internal/store/working.go` — add `AuditForSubject` (audit lives here, not in `queries.go`)
- `internal/httpapi/server.go` — register 5 new routes, extend `handleSessionStart` with `persona_block`
- `internal/mcp/server.go` — register 5 new tools, extend `anamnesia_audit` with `subject` arg
- `cmd/anamnesia/hook.go` — `doSessionStart` renders `## How to respond` above `## Anamnesia memory`
- `cmd/anamnesia/serve.go` — construct `jobs.Briefer` and inject into both `httpapi.Deps` and `mcp.Deps`

---

## Task 1: Identity primitive

**Files:**
- Create: `pkg/anamnesia/identity.go`
- Create: `internal/store/identity.go`
- Create: `internal/store/identity_render.go`
- Create: `internal/store/identity_render_test.go`
- Modify: `internal/httpapi/server.go`
- Modify: `internal/mcp/server.go`
- Modify: `cmd/anamnesia/hook.go`

- [ ] **Step 1.1: Define the public Identity type**

Create `pkg/anamnesia/identity.go`:

```go
package anamnesia

// Identity is the boot-shaped view of who the user is. Persona drives
// voice ("how to respond"); Profile is biographical fact ("who they are").
// SystemPrompt is the rendered block dock-on agents put at the top of
// their system message.
type Identity struct {
	Scope        Scope          `json:"scope"`
	Persona      map[string]any `json:"persona"`
	Profile      map[string]any `json:"profile"`
	SystemPrompt string         `json:"system_prompt"`
}
```

- [ ] **Step 1.2: Write the failing render test**

Create `internal/store/identity_render_test.go`:

```go
package store

import (
	"strings"
	"testing"

	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

func TestRenderIdentity_VerbatimSystemPromptWins(t *testing.T) {
	id := anamnesia.Identity{
		Persona: map[string]any{
			"system_prompt": "Speak in short, direct sentences.",
			"tone":          "warm but terse",
		},
		Profile: map[string]any{"name": "Florian", "timezone": "Europe/Berlin"},
	}
	got := RenderIdentity(id)
	if !strings.HasPrefix(got, "Speak in short, direct sentences.") {
		t.Fatalf("verbatim system_prompt should lead the render, got:\n%s", got)
	}
	if !strings.Contains(got, "tone: warm but terse") {
		t.Fatalf("other persona keys missing: %s", got)
	}
	if !strings.Contains(got, "### About me") {
		t.Fatalf("profile section header missing: %s", got)
	}
	if !strings.Contains(got, "- name: Florian") {
		t.Fatalf("profile bullet missing: %s", got)
	}
}

func TestRenderIdentity_EmptyReturnsEmpty(t *testing.T) {
	if got := RenderIdentity(anamnesia.Identity{}); got != "" {
		t.Fatalf("empty identity must render empty, got %q", got)
	}
}
```

- [ ] **Step 1.3: Run the test, verify it fails**

Run: `cd /Users/floh/Private/anamnesia-open-source && go test ./internal/store/ -run TestRenderIdentity`
Expected: FAIL with `undefined: RenderIdentity`.

- [ ] **Step 1.4: Implement RenderIdentity**

Create `internal/store/identity_render.go`:

```go
package store

import (
	"fmt"
	"sort"
	"strings"

	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

// RenderIdentity produces the deterministic system-prompt block that
// every dock-on agent puts at the top of its system message. It is the
// single source of truth for "how to talk to this user".
//
// Order:
//  1. user.persona.system_prompt verbatim (if present)
//  2. other persona keys as "<key>: <value>" lines, sorted
//  3. separator
//  4. "### About me" + profile keys as "- <key>: <value>" bullets, sorted
//
// Empty input produces empty output.
func RenderIdentity(id anamnesia.Identity) string {
	if len(id.Persona) == 0 && len(id.Profile) == 0 {
		return ""
	}
	var b strings.Builder
	if sp, ok := id.Persona["system_prompt"]; ok {
		if s, ok := sp.(string); ok && strings.TrimSpace(s) != "" {
			b.WriteString(strings.TrimSpace(s))
			b.WriteString("\n")
		}
	}
	otherPersonaKeys := make([]string, 0, len(id.Persona))
	for k := range id.Persona {
		if k == "system_prompt" {
			continue
		}
		otherPersonaKeys = append(otherPersonaKeys, k)
	}
	sort.Strings(otherPersonaKeys)
	for _, k := range otherPersonaKeys {
		fmt.Fprintf(&b, "%s: %v\n", k, id.Persona[k])
	}
	if len(id.Profile) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("### About me\n")
		profileKeys := make([]string, 0, len(id.Profile))
		for k := range id.Profile {
			profileKeys = append(profileKeys, k)
		}
		sort.Strings(profileKeys)
		for _, k := range profileKeys {
			fmt.Fprintf(&b, "- %s: %v\n", k, id.Profile[k])
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
```

- [ ] **Step 1.5: Run test, verify PASS**

Run: `go test ./internal/store/ -run TestRenderIdentity -v`
Expected: PASS for both tests.

- [ ] **Step 1.6: Add Store.GetIdentity**

Create `internal/store/identity.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

// GetIdentity returns the user's Identity by scanning facts under the
// reserved key prefixes user.persona.* and user.profile.*. The render
// is filled in by RenderIdentity.
//
// Scope.ProjectID is ignored deliberately: identity is a user-level
// concept and follows the user across projects.
func (s *Store) GetIdentity(ctx context.Context, scope anamnesia.Scope) (anamnesia.Identity, error) {
	out := anamnesia.Identity{
		Scope:   anamnesia.Scope{UserID: scope.UserID},
		Persona: map[string]any{},
		Profile: map[string]any{},
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT key, value
		FROM facts
		WHERE user_id = $1
		  AND deleted_at IS NULL
		  AND (key LIKE 'user.persona.%' OR key LIKE 'user.profile.%')
		ORDER BY ingested_at DESC`, scope.UserID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	// Newest-first wins per key (later iterations skip seen keys).
	seen := map[string]bool{}
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return out, err
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		val := unpackFactValue(raw)
		switch {
		case strings.HasPrefix(key, "user.persona."):
			out.Persona[strings.TrimPrefix(key, "user.persona.")] = val
		case strings.HasPrefix(key, "user.profile."):
			out.Profile[strings.TrimPrefix(key, "user.profile.")] = val
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.SystemPrompt = RenderIdentity(out)
	return out, nil
}

// unpackFactValue returns the most useful inner type from a fact value
// jsonb blob. Facts are stored as JSON objects; persona/profile values
// are usually a single primitive — return the "v" key if it exists,
// else the whole object.
func unpackFactValue(raw []byte) any {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return string(raw)
	}
	if v, ok := obj["v"]; ok {
		return v
	}
	return obj
}
```

- [ ] **Step 1.7: Wire the HTTP route**

In `internal/httpapi/server.go`, register the route inside `NewServer`, immediately after the `/v1/experience` mux.Handle line:

```go
mux.Handle("/v1/identity", d.protect(http.HandlerFunc(d.handleIdentity)))
```

Add the handler in the "handlers" section (just before the `helpers` block at the bottom):

```go
func (d Deps) handleIdentity(w http.ResponseWriter, r *http.Request) {
	ev := HookEvent{
		User:    r.URL.Query().Get("user"),
		Project: r.URL.Query().Get("project"),
	}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, err := d.Store.GetIdentity(r.Context(), scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, id)
}
```

- [ ] **Step 1.8: Extend SessionStart with persona_block**

In `internal/httpapi/server.go`, modify `SessionStartResp`:

```go
type SessionStartResp struct {
	Facts        []*anamnesia.Fact       `json:"facts"`
	Experiences  []*anamnesia.Experience `json:"experiences"`
	PersonaBlock string                  `json:"persona_block,omitempty"`
	Hint         string                  `json:"hint,omitempty"`
}
```

Change the final `writeJSON(...)` line of `handleSessionStart` to also fetch identity and return the block:

```go
id, err := d.Store.GetIdentity(r.Context(), scope)
if err != nil {
	http.Error(w, err.Error(), http.StatusInternalServerError)
	return
}
writeJSON(w, http.StatusOK, SessionStartResp{
	Facts: facts, Experiences: exps, PersonaBlock: id.SystemPrompt,
})
```

- [ ] **Step 1.9: Wire the MCP tool**

In `internal/mcp/server.go`, inside `registerTools`, add right after the existing `anamnesia_search` block:

```go
s.AddTool(
	mcp.NewTool("anamnesia_identity",
		mcp.WithDescription("Return the user's identity: persona + profile + a rendered system_prompt block. Dock-on agents call this at boot."),
		mcp.WithString("user"), mcp.WithString("project"),
	),
	d.identity,
)
```

Add the handler near the other handlers:

```go
func (d Deps) identity(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	id, err := d.Store.GetIdentity(ctx, scope)
	if err != nil {
		return bad(err)
	}
	return ok(id)
}
```

- [ ] **Step 1.10: Hook renders "How to respond"**

In `cmd/anamnesia/hook.go`, add the field to `sessionStartResp`:

```go
type sessionStartResp struct {
	Facts        []factMin       `json:"facts"`
	Experiences  []experienceMin `json:"experiences"`
	PersonaBlock string          `json:"persona_block,omitempty"`
	Hint         string          `json:"hint,omitempty"`
}
```

Replace the early-return + memory-block emission at the bottom of `doSessionStart` (starting at the existing `if len(resp.Facts) == 0 && len(resp.Experiences) == 0` line) with:

```go
hasPersona := strings.TrimSpace(resp.PersonaBlock) != ""
hasMemory := len(resp.Facts) > 0 || len(resp.Experiences) > 0
if !hasPersona && !hasMemory {
	return nil
}
if hasPersona {
	fmt.Fprintln(w, "## How to respond")
	fmt.Fprintln(w, resp.PersonaBlock)
	fmt.Fprintln(w)
}
if !hasMemory {
	return nil
}
fmt.Fprintln(w, "## Anamnesia memory")
if len(resp.Facts) > 0 {
	fmt.Fprintln(w, "\n**Facts**")
	for _, f := range resp.Facts {
		val, _ := json.Marshal(f.Value)
		fmt.Fprintf(w, "- `%s` (%s): %s\n", f.Key, f.FactKind, string(val))
	}
}
if len(resp.Experiences) > 0 {
	fmt.Fprintln(w, "\n**Recent experiences**")
	for _, e := range resp.Experiences {
		title := e.Title
		if title == "" {
			title = trimLine(e.Body, 80)
		}
		fmt.Fprintf(w, "- %s\n", title)
	}
}
return nil
```

- [ ] **Step 1.11: Build, run, verify**

```bash
cd /Users/floh/Private/anamnesia-open-source
go build ./...
go test ./internal/store/ -run TestRenderIdentity -v
./bin/anamnesia up
./bin/anamnesia doctor
# Seed a persona fact via MCP:
curl -sS http://localhost:8181/mcp/ \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"anamnesia_facts_upsert","arguments":{"key":"user.persona.system_prompt","value":{"v":"Be terse. Skip pleasantries."},"scope":"user"}}}'
# Read back:
curl -sS 'http://localhost:8181/v1/identity?user=default'
# Expected: JSON with system_prompt = "Be terse. Skip pleasantries."
```

- [ ] **Step 1.12: Commit**

```bash
git add pkg/anamnesia/identity.go internal/store/identity.go internal/store/identity_render.go internal/store/identity_render_test.go internal/httpapi/server.go internal/mcp/server.go cmd/anamnesia/hook.go
git commit -m "feat(substrate): identity primitive — persona+profile read surface, SessionStart 'How to respond' header"
```

---

## Task 2: Briefing primitive

**Files:**
- Create: `pkg/anamnesia/briefing.go`
- Modify: `internal/store/experiences.go` (add `ListExperiencesInWindow`)
- Create: `internal/jobs/briefing.go`
- Create: `internal/jobs/briefing_test.go`
- Modify: `internal/httpapi/server.go`
- Modify: `internal/mcp/server.go`
- Modify: `cmd/anamnesia/serve.go`

- [ ] **Step 2.1: Define the public types**

Create `pkg/anamnesia/briefing.go`:

```go
package anamnesia

import "time"

type BriefingRequest struct {
	Scope       Scope     `json:"scope"`
	Since       time.Time `json:"since"`
	Until       time.Time `json:"until,omitempty"`  // zero = now
	Topic       string    `json:"topic,omitempty"`
	MaxAdjacent int       `json:"max_adjacent,omitempty"` // default 3
}

type BriefingResponse struct {
	Window     Window         `json:"window"`
	Summary    string         `json:"summary"`
	Highlights []BriefingItem `json:"highlights"`
	Adjacent   []BriefingItem `json:"adjacent"`
}

type Window struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

type BriefingItem struct {
	Title string `json:"title"`
	Why   string `json:"why"`
	Kind  string `json:"kind,omitempty"`
}
```

- [ ] **Step 2.2: Add ListExperiencesInWindow to the store**

Before writing, read `internal/store/experiences.go` to confirm the exact column list `scanExperience` expects — the SELECT below must match it byte-for-byte. Then append the function:

```go
// ListExperiencesInWindow returns non-deleted experiences whose
// occurred_at falls in [since, until]. ProjectID, if set, filters
// further. Topic filter is prefix-match on title or topic column.
func (s *Store) ListExperiencesInWindow(
	ctx context.Context,
	scope anamnesia.Scope,
	since, until time.Time,
	topic string,
	limit int,
) ([]*anamnesia.Experience, error) {
	if limit <= 0 {
		limit = 200
	}
	if until.IsZero() {
		until = time.Now().UTC()
	}
	args := []any{scope.UserID, since, until}
	where := []string{
		"user_id = $1",
		"deleted_at IS NULL",
		"occurred_at >= $2",
		"occurred_at <= $3",
	}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("project_id = $%d", len(args)))
	}
	if topic != "" {
		args = append(args, topic+"%")
		where = append(where, fmt.Sprintf("(title ILIKE $%d OR topic ILIKE $%d)",
			len(args), len(args)))
	}
	args = append(args, limit)
	// Column list must match scanExperience exactly — see ListExperiences in this file.
	q := fmt.Sprintf(`
		SELECT id, user_id, project_id, source_id, kind, abstraction,
		       title, body, outcome, meta,
		       trust, importance, relevance, use_count, last_used_at, pii_tags,
		       embed_model, valid_from, valid_to, ingested_at, invalidated_at,
		       superseded_by, deleted_at, occurred_at, participants, topic,
		       parent_id, provenance
		FROM experiences WHERE %s
		ORDER BY occurred_at ASC
		LIMIT $%d`, strings.Join(where, " AND "), len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Experience
	for rows.Next() {
		e, err := scanExperience(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
```

If `experiences.go`'s existing `ListExperiences` SELECT differs from the column list above, replace the column list to match it verbatim — `scanExperience` is the contract.

- [ ] **Step 2.3: Confirm llm.Client interface, then write failing test**

```bash
grep -n "type Client" internal/llm/llm.go
```

Adjust the fake LLM in the test below if `Distill` / `Extract` signatures differ. Then create `internal/jobs/briefing_test.go`:

```go
package jobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia-open-source/internal/llm"
	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

type fakeBriefingLLM struct {
	gotUser string
	out     string
}

func (f *fakeBriefingLLM) Model() string                                        { return "fake" }
func (f *fakeBriefingLLM) Complete(_ context.Context, _ string) (string, error) { return f.out, nil }
func (f *fakeBriefingLLM) Distill(_ context.Context, in llm.DistillInput, _ any) error {
	f.gotUser = in.User
	return nil
}
func (f *fakeBriefingLLM) Extract(_ context.Context, in llm.DistillInput, _ any) error {
	f.gotUser = in.User
	return nil
}

func TestBriefingPrompt_IncludesWindowAndExperiences(t *testing.T) {
	llmFake := &fakeBriefingLLM{out: `{"summary":"...","highlights":[],"adjacent":[]}`}
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
	_, err := b.Brief(context.Background(), anamnesia.BriefingRequest{
		Since: since, Until: until, MaxAdjacent: 3,
	}, exps, adj)
	if err != nil {
		t.Fatalf("brief: %v", err)
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
```

- [ ] **Step 2.4: Implement Briefer**

Create `internal/jobs/briefing.go`:

```go
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/flohs/anamnesia-open-source/internal/llm"
	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

const briefingSystemPrompt = `You are a personal-assistant brief writer. Given a list of experiences in a time window plus "adjacent" experiences from before or related topics, produce JSON:
{"summary":"<one paragraph, plain prose>","highlights":[{"title":"...","why":"..."}],"adjacent":[{"title":"...","why":"you might also want to mention this because ...","kind":"experience"}]}
Limit highlights to <=5 and adjacent to the MaxAdjacent provided. No prose outside JSON.`

type Briefer struct {
	LLM llm.Client
}

func (b Briefer) Brief(
	ctx context.Context,
	req anamnesia.BriefingRequest,
	inWindow, adjacent []*anamnesia.Experience,
) (anamnesia.BriefingResponse, error) {
	maxAdj := req.MaxAdjacent
	if maxAdj == 0 {
		maxAdj = 3
	}
	user := fmt.Sprintf("Window: %s — %s\nMaxAdjacent: %d\nTopic: %q\n\n",
		req.Since.Format("2006-01-02"), req.Until.Format("2006-01-02"), maxAdj, req.Topic)
	user += "In-window experiences:\n" + renderExperiencesForPrompt(inWindow) + "\n"
	user += "Adjacent experiences:\n" + renderExperiencesForPrompt(adjacent) + "\n"
	user += "\nRespond with the JSON object only."

	out := anamnesia.BriefingResponse{
		Window: anamnesia.Window{Since: req.Since, Until: req.Until},
	}
	if b.LLM == nil {
		out.Summary = fmt.Sprintf("(stub) %d experiences in window, %d adjacent",
			len(inWindow), len(adjacent))
		return out, nil
	}
	var raw json.RawMessage
	if err := b.LLM.Distill(ctx, llm.DistillInput{
		System: briefingSystemPrompt,
		User:   user,
	}, &raw); err != nil {
		return out, fmt.Errorf("briefing llm: %w", err)
	}
	var parsed struct {
		Summary    string                   `json:"summary"`
		Highlights []anamnesia.BriefingItem `json:"highlights"`
		Adjacent   []anamnesia.BriefingItem `json:"adjacent"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return out, fmt.Errorf("briefing parse: %w (raw=%s)", err, string(raw))
	}
	out.Summary = parsed.Summary
	out.Highlights = parsed.Highlights
	out.Adjacent = parsed.Adjacent
	if len(out.Adjacent) > maxAdj {
		out.Adjacent = out.Adjacent[:maxAdj]
	}
	return out, nil
}

func renderExperiencesForPrompt(es []*anamnesia.Experience) string {
	if len(es) == 0 {
		return "  (none)\n"
	}
	var b strings.Builder
	for _, e := range es {
		when := ""
		if e.OccurredAt != nil {
			when = e.OccurredAt.Format("2006-01-02") + " — "
		}
		fmt.Fprintf(&b, "  - %s%s | %s\n", when, e.Title, firstLine(e.Body, 200))
	}
	return b.String()
}

func firstLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
```

If `llm.DistillInput` has a different field name than `User` (e.g. `Prompt`), adjust both the implementation and the test fake; they share the same constant.

- [ ] **Step 2.5: Run tests, verify PASS**

```bash
go build ./...
go test ./internal/jobs/ -run TestBriefingPrompt -v
```

Expected: PASS.

- [ ] **Step 2.6: Wire HTTP route**

In `internal/httpapi/server.go`, add an import:

```go
"github.com/flohs/anamnesia-open-source/internal/jobs"
```

Add a field to `Deps`:

```go
Briefer *jobs.Briefer // nil-safe; nil falls back to a stub summary
```

Register the route:

```go
mux.Handle("/v1/briefing", d.protect(http.HandlerFunc(d.handleBriefing)))
```

Add the handler:

```go
type briefingReq struct {
	User        string    `json:"user,omitempty"`
	Project     string    `json:"project,omitempty"`
	Since       time.Time `json:"since"`
	Until       time.Time `json:"until,omitempty"`
	Topic       string    `json:"topic,omitempty"`
	MaxAdjacent int       `json:"max_adjacent,omitempty"`
}

func (d Deps) handleBriefing(w http.ResponseWriter, r *http.Request) {
	var req briefingReq
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Since.IsZero() {
		http.Error(w, "since required", http.StatusBadRequest)
		return
	}
	ev := HookEvent{User: req.User, Project: req.Project}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	inWin, err := d.Store.ListExperiencesInWindow(r.Context(), scope, req.Since, req.Until, req.Topic, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Adjacent = ±14d flanking the request window, capped.
	flankSince := req.Since.AddDate(0, 0, -14)
	flankUntil := req.Until
	if flankUntil.IsZero() {
		flankUntil = time.Now().UTC()
	}
	flankUntil = flankUntil.AddDate(0, 0, 14)
	adj, err := d.Store.ListExperiencesInWindow(r.Context(), scope, flankSince, flankUntil, req.Topic, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Filter in-window experiences out of adjacent.
	inSet := map[uuid.UUID]bool{}
	for _, e := range inWin {
		inSet[e.ID] = true
	}
	adjOut := adj[:0]
	for _, e := range adj {
		if !inSet[e.ID] {
			adjOut = append(adjOut, e)
		}
	}
	var briefer jobs.Briefer
	if d.Briefer != nil {
		briefer = *d.Briefer
	}
	resp, err := briefer.Brief(r.Context(), anamnesia.BriefingRequest{
		Scope: scope, Since: req.Since, Until: req.Until,
		Topic: req.Topic, MaxAdjacent: req.MaxAdjacent,
	}, inWin, adjOut)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
```

- [ ] **Step 2.7: Construct Briefer in serve.go**

Read `cmd/anamnesia/serve.go` to find where `httpapi.Deps` is constructed and `llm.Client` is built. Add:

```go
briefer := &jobs.Briefer{LLM: llmClient}
// ... include `Briefer: briefer,` in the httpapi.Deps literal,
// and `Briefer: briefer,` in the mcp.Deps literal.
```

If `llmClient` is unnamed (inline expression), pull it into a named variable first.

- [ ] **Step 2.8: Wire MCP tool**

In `internal/mcp/server.go`, add a field to `Deps`:

```go
Briefer *jobs.Briefer
```

Add imports:

```go
"time"
"github.com/flohs/anamnesia-open-source/internal/jobs"
```

Register the tool:

```go
s.AddTool(
	mcp.NewTool("anamnesia_briefing",
		mcp.WithDescription("Summarise experiences in a time window with 'adjacent' items the user might want to mention. Returns {summary, highlights[], adjacent[]}."),
		mcp.WithString("since", mcp.Required(), mcp.Description("RFC3339 timestamp")),
		mcp.WithString("until", mcp.Description("RFC3339; default now")),
		mcp.WithString("topic"),
		mcp.WithNumber("max_adjacent"),
		mcp.WithString("user"), mcp.WithString("project"),
	),
	d.briefing,
)
```

Add the handler:

```go
func (d Deps) briefing(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	since, err := time.Parse(time.RFC3339, argString(args, "since"))
	if err != nil {
		return bad(fmt.Errorf("since: %w", err))
	}
	var until time.Time
	if u := argString(args, "until"); u != "" {
		until, err = time.Parse(time.RFC3339, u)
		if err != nil {
			return bad(fmt.Errorf("until: %w", err))
		}
	}
	topic := argString(args, "topic")
	inWin, err := d.Store.ListExperiencesInWindow(ctx, scope, since, until, topic, 200)
	if err != nil {
		return bad(err)
	}
	flankSince := since.AddDate(0, 0, -14)
	flankUntil := until
	if flankUntil.IsZero() {
		flankUntil = time.Now().UTC()
	}
	flankUntil = flankUntil.AddDate(0, 0, 14)
	adj, err := d.Store.ListExperiencesInWindow(ctx, scope, flankSince, flankUntil, topic, 50)
	if err != nil {
		return bad(err)
	}
	inSet := map[uuid.UUID]bool{}
	for _, e := range inWin {
		inSet[e.ID] = true
	}
	adjOut := adj[:0]
	for _, e := range adj {
		if !inSet[e.ID] {
			adjOut = append(adjOut, e)
		}
	}
	var briefer jobs.Briefer
	if d.Briefer != nil {
		briefer = *d.Briefer
	}
	out, err := briefer.Brief(ctx, anamnesia.BriefingRequest{
		Scope: scope, Since: since, Until: until,
		Topic: topic, MaxAdjacent: argInt(args, "max_adjacent", 3),
	}, inWin, adjOut)
	if err != nil {
		return bad(err)
	}
	return ok(out)
}
```

- [ ] **Step 2.9: Compile and verify**

```bash
go build ./...
go test ./internal/jobs/ ./internal/store/ -v
./bin/anamnesia up
curl -sS -X POST http://localhost:8181/v1/briefing \
  -H 'Content-Type: application/json' \
  -d '{"user":"default","since":"2026-05-20T00:00:00Z"}'
# Expected: JSON {window, summary, highlights:[], adjacent:[]}.
# With no experiences, summary will be the stub line.
```

- [ ] **Step 2.10: Commit**

```bash
git add pkg/anamnesia/briefing.go internal/store/experiences.go internal/jobs/briefing.go internal/jobs/briefing_test.go internal/httpapi/server.go internal/mcp/server.go cmd/anamnesia/serve.go
git commit -m "feat(substrate): briefing primitive — temporal-window summary with adjacency"
```

---

## Task 3: People surface

**Files:**
- Create: `pkg/anamnesia/people.go`
- Create: `internal/store/people.go`
- Modify: `internal/httpapi/server.go`
- Modify: `internal/mcp/server.go`

- [ ] **Step 3.1: Define PersonView**

Create `pkg/anamnesia/people.go`:

```go
package anamnesia

import "time"

type PersonView struct {
	Entity          *Entity      `json:"entity"`
	RecentMentions  int          `json:"recent_mentions"`
	LastMentionedAt *time.Time   `json:"last_mentioned_at,omitempty"`
	Edges           []PersonEdge `json:"edges,omitempty"`
}

type PersonEdge struct {
	Kind   string `json:"kind"`
	ToName string `json:"to_name"`
	ToID   string `json:"to_id"`
}
```

- [ ] **Step 3.2: Read Neighbors() signature**

Before writing `ListPeople`, read `internal/store/graph.go` to confirm the exact return shape of `Store.Neighbors(ctx, id, kinds, dir, limit)`. The implementation below assumes it returns `([]*Entity, []*Edge, error)` — adjust if it differs.

- [ ] **Step 3.3: Implement ListPeople**

Create `internal/store/people.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

// ListPeople returns entities of kind="person" within scope, decorated
// with recent-mention counts (last 90d) and a small slice of outgoing edges.
func (s *Store) ListPeople(ctx context.Context, scope anamnesia.Scope, limit int) ([]anamnesia.PersonView, error) {
	if limit <= 0 {
		limit = 50
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -90)
	rows, err := s.Pool.Query(ctx, `
		SELECT e.id, e.user_id, e.project_id, e.kind, e.name, e.props, e.created_at,
		       (SELECT count(*) FROM experiences x
		         WHERE x.user_id = e.user_id
		           AND x.deleted_at IS NULL
		           AND x.occurred_at >= $2
		           AND e.name = ANY(x.participants)) AS recent_mentions,
		       (SELECT max(x.occurred_at) FROM experiences x
		         WHERE x.user_id = e.user_id
		           AND x.deleted_at IS NULL
		           AND e.name = ANY(x.participants)) AS last_mentioned_at
		FROM entities e
		WHERE e.user_id = $1
		  AND e.kind = 'person'
		ORDER BY recent_mentions DESC NULLS LAST, e.name ASC
		LIMIT $3`, scope.UserID, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []anamnesia.PersonView
	for rows.Next() {
		var (
			ent      anamnesia.Entity
			props    []byte
			project  *uuid.UUID
			mentions int
			last     *time.Time
		)
		if err := rows.Scan(&ent.ID, &ent.Scope.UserID, &project, &ent.Kind,
			&ent.Name, &props, &ent.CreatedAt, &mentions, &last); err != nil {
			return nil, err
		}
		ent.Scope.ProjectID = project
		if len(props) > 0 {
			_ = json.Unmarshal(props, &ent.Props)
		}
		view := anamnesia.PersonView{
			Entity: &ent, RecentMentions: mentions, LastMentionedAt: last,
		}
		// Fetch outbound edges (cap at 5 per person).
		neighbors, edges, _ := s.Neighbors(ctx, ent.ID, nil, "out", 5)
		nameByID := map[uuid.UUID]string{}
		for _, n := range neighbors {
			nameByID[n.ID] = n.Name
		}
		for _, e := range edges {
			view.Edges = append(view.Edges, anamnesia.PersonEdge{
				Kind: e.Kind, ToName: nameByID[e.To], ToID: e.To.String(),
			})
		}
		out = append(out, view)
	}
	return out, rows.Err()
}
```

Project-scoping is intentionally not applied here: people live at the user level, and a person mentioned in one project is still that person in another. If the spec changes to require project-scoping later, add a filter.

- [ ] **Step 3.4: Wire HTTP + MCP**

HTTP (in `internal/httpapi/server.go`):

```go
mux.Handle("/v1/people", d.protect(http.HandlerFunc(d.handlePeople)))

func (d Deps) handlePeople(w http.ResponseWriter, r *http.Request) {
	ev := HookEvent{
		User: r.URL.Query().Get("user"), Project: r.URL.Query().Get("project"),
	}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	people, err := d.Store.ListPeople(r.Context(), scope, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, people)
}
```

MCP (in `internal/mcp/server.go`):

```go
s.AddTool(
	mcp.NewTool("anamnesia_people",
		mcp.WithDescription("List people the user knows, sorted by recent-mention count."),
		mcp.WithString("user"), mcp.WithString("project"),
		mcp.WithNumber("limit"),
	),
	d.people,
)

func (d Deps) people(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	out, err := d.Store.ListPeople(ctx, scope, argInt(args, "limit", 50))
	if err != nil {
		return bad(err)
	}
	return ok(out)
}
```

- [ ] **Step 3.5: Build, seed, verify**

```bash
go build ./...
curl -sS http://localhost:8181/mcp/ -H 'Content-Type: application/json' -d \
  '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"anamnesia_graph_entity","arguments":{"kind":"person","name":"Sarah"}}}'
curl -sS 'http://localhost:8181/v1/people?user=default'
# Expected: JSON array with one entry for Sarah.
```

- [ ] **Step 3.6: Commit**

```bash
git add pkg/anamnesia/people.go internal/store/people.go internal/httpapi/server.go internal/mcp/server.go
git commit -m "feat(substrate): people surface — relationships read view with recent-mention counts"
```

---

## Task 4: Capabilities surface

**Files:**
- Create: `internal/store/capabilities.go`
- Modify: `internal/httpapi/server.go`
- Modify: `internal/mcp/server.go`

- [ ] **Step 4.1: Implement ListCapabilities**

Create `internal/store/capabilities.go`:

```go
package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

// ListCapabilities returns active skills in scope, sorted by freshness
// (last_used_at DESC NULLS LAST, then use_count DESC). Boot-shaped view
// — agents call this to discover what they can lean on.
func (s *Store) ListCapabilities(ctx context.Context, scope anamnesia.Scope, limit int) ([]*anamnesia.Skill, error) {
	if limit <= 0 {
		limit = 50
	}
	args := []any{scope.UserID}
	where := []string{"user_id = $1", "deleted_at IS NULL"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("(project_id = $%d OR project_id IS NULL)", len(args)))
	}
	args = append(args, limit)
	// Column list must match scanSkill() in skills.go — read it to confirm.
	q := fmt.Sprintf(`
		SELECT id, user_id, project_id, name, kind, description, signature, body, meta,
		       use_count, last_used_at, deleted_at
		FROM skills WHERE %s
		ORDER BY last_used_at DESC NULLS LAST, use_count DESC, name ASC
		LIMIT $%d`, strings.Join(where, " AND "), len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Skill
	for rows.Next() {
		sk, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}
```

If `scanSkill`'s column expectations differ from the SELECT above, replace the column list to match.

- [ ] **Step 4.2: Wire HTTP**

In `internal/httpapi/server.go`:

```go
mux.Handle("/v1/capabilities", d.protect(http.HandlerFunc(d.handleCapabilities)))

func (d Deps) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	ev := HookEvent{User: r.URL.Query().Get("user"), Project: r.URL.Query().Get("project")}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	out, err := d.Store.ListCapabilities(r.Context(), scope, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
```

- [ ] **Step 4.3: Wire MCP**

In `internal/mcp/server.go`:

```go
s.AddTool(
	mcp.NewTool("anamnesia_capabilities",
		mcp.WithDescription("List skills/tools registered for this user, freshness-ordered. Use at boot to discover what you can call."),
		mcp.WithString("user"), mcp.WithString("project"),
		mcp.WithNumber("limit"),
	),
	d.capabilities,
)

func (d Deps) capabilities(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	out, err := d.Store.ListCapabilities(ctx, scope, argInt(args, "limit", 50))
	if err != nil {
		return bad(err)
	}
	return ok(out)
}
```

- [ ] **Step 4.4: Build, verify, commit**

```bash
go build ./...
curl -sS 'http://localhost:8181/v1/capabilities?user=default'
git add internal/store/capabilities.go internal/httpapi/server.go internal/mcp/server.go
git commit -m "feat(substrate): capabilities surface — boot-shaped freshness view over skills"
```

---

## Task 5: Commitments domain

**Files:**
- Create: `internal/store/migrations/0007_commitments.sql`
- Create: `pkg/anamnesia/commitments.go`
- Create: `internal/store/commitments.go`
- Modify: `internal/httpapi/server.go`
- Modify: `internal/mcp/server.go`

- [ ] **Step 5.1: Migration**

Create `internal/store/migrations/0007_commitments.sql`:

```sql
-- +goose Up
CREATE TABLE commitments (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users(id),
    project_id  uuid REFERENCES projects(id),
    owner       text NOT NULL,
    beneficiary text NOT NULL,
    body        text NOT NULL,
    due_at      timestamptz,
    status      text NOT NULL DEFAULT 'open'
                CHECK (status IN ('open','done','dropped')),
    source_id   uuid REFERENCES sources(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX commitments_user_status_due
    ON commitments (user_id, status, due_at);
CREATE INDEX commitments_project_status
    ON commitments (project_id, status)
    WHERE project_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS commitments;
```

- [ ] **Step 5.2: Public type**

Create `pkg/anamnesia/commitments.go`:

```go
package anamnesia

import (
	"time"

	"github.com/google/uuid"
)

type CommitmentStatus string

const (
	CommitmentOpen    CommitmentStatus = "open"
	CommitmentDone    CommitmentStatus = "done"
	CommitmentDropped CommitmentStatus = "dropped"
)

func (s CommitmentStatus) Valid() bool {
	switch s {
	case CommitmentOpen, CommitmentDone, CommitmentDropped:
		return true
	}
	return false
}

type Commitment struct {
	ID          uuid.UUID        `json:"id"`
	Scope       Scope            `json:"scope"`
	Owner       string           `json:"owner"`
	Beneficiary string           `json:"beneficiary"`
	Body        string           `json:"body"`
	DueAt       *time.Time       `json:"due_at,omitempty"`
	Status      CommitmentStatus `json:"status"`
	SourceID    *uuid.UUID       `json:"source_id,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}
```

- [ ] **Step 5.3: Store methods**

Create `internal/store/commitments.go`:

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

func (s *Store) RecordCommitment(ctx context.Context, c *anamnesia.Commitment) error {
	if c.Body == "" {
		return errors.New("commitment: body required")
	}
	if c.Owner == "" {
		c.Owner = "user"
	}
	if c.Beneficiary == "" {
		c.Beneficiary = "user"
	}
	if !c.Status.Valid() {
		c.Status = anamnesia.CommitmentOpen
	}
	return s.Pool.QueryRow(ctx, `
		INSERT INTO commitments
			(user_id, project_id, owner, beneficiary, body, due_at, status, source_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at, updated_at`,
		c.Scope.UserID, c.Scope.ProjectID, c.Owner, c.Beneficiary,
		c.Body, c.DueAt, string(c.Status), c.SourceID,
	).Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt)
}

func (s *Store) ListCommitments(
	ctx context.Context, scope anamnesia.Scope,
	status anamnesia.CommitmentStatus, limit int,
) ([]*anamnesia.Commitment, error) {
	if limit <= 0 {
		limit = 50
	}
	args := []any{scope.UserID}
	where := []string{"user_id = $1"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("project_id = $%d", len(args)))
	}
	if status != "" {
		args = append(args, string(status))
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT id, user_id, project_id, owner, beneficiary, body, due_at,
		       status, source_id, created_at, updated_at
		FROM commitments WHERE %s
		ORDER BY (status = 'open') DESC,
		         due_at ASC NULLS LAST,
		         created_at DESC
		LIMIT $%d`, strings.Join(where, " AND "), len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Commitment
	for rows.Next() {
		c, err := scanCommitment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ResolveCommitment(ctx context.Context, id uuid.UUID, status anamnesia.CommitmentStatus) error {
	if !status.Valid() || status == anamnesia.CommitmentOpen {
		return fmt.Errorf("resolve status must be done or dropped, got %q", status)
	}
	tag, err := s.Pool.Exec(ctx,
		`UPDATE commitments SET status = $2, updated_at = now() WHERE id = $1`,
		id, string(status))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanCommitment(row rowScanner) (*anamnesia.Commitment, error) {
	var (
		c       anamnesia.Commitment
		project *uuid.UUID
		due     *time.Time
		status  string
		srcID   *uuid.UUID
	)
	err := row.Scan(&c.ID, &c.Scope.UserID, &project, &c.Owner, &c.Beneficiary,
		&c.Body, &due, &status, &srcID, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Scope.ProjectID = project
	c.DueAt = due
	c.Status = anamnesia.CommitmentStatus(status)
	c.SourceID = srcID
	return &c, nil
}
```

- [ ] **Step 5.4: Run migration, smoke-test the SQL**

```bash
./bin/anamnesia migrate
# (Or restart the stack so goose runs automatically.)
docker compose exec postgres psql -U anamnesia -d anamnesia -c "\d commitments"
# Expected: table with the columns above.
```

- [ ] **Step 5.5: HTTP routes**

In `internal/httpapi/server.go`:

```go
mux.Handle("/v1/commitments", d.protect(http.HandlerFunc(d.handleCommitments)))
mux.Handle("/v1/commitments/resolve", d.protect(http.HandlerFunc(d.handleCommitmentResolve)))

type commitmentReq struct {
	User        string     `json:"user,omitempty"`
	Project     string     `json:"project,omitempty"`
	Owner       string     `json:"owner,omitempty"`
	Beneficiary string     `json:"beneficiary,omitempty"`
	Body        string     `json:"body"`
	DueAt       *time.Time `json:"due_at,omitempty"`
}

func (d Deps) handleCommitments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ev := HookEvent{User: r.URL.Query().Get("user"), Project: r.URL.Query().Get("project")}
		scope, err := d.resolveScope(r.Context(), &ev)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		status := anamnesia.CommitmentStatus(r.URL.Query().Get("status"))
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			fmt.Sscanf(v, "%d", &limit)
		}
		out, err := d.Store.ListCommitments(r.Context(), scope, status, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var req commitmentReq
		if err := readJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Body == "" {
			http.Error(w, "body required", http.StatusBadRequest)
			return
		}
		scope, err := d.resolveScope(r.Context(), &HookEvent{User: req.User, Project: req.Project})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c := &anamnesia.Commitment{
			Scope: scope, Owner: req.Owner, Beneficiary: req.Beneficiary,
			Body: req.Body, DueAt: req.DueAt, Status: anamnesia.CommitmentOpen,
		}
		if err := d.Store.RecordCommitment(r.Context(), c); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, c)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type resolveReq struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (d Deps) handleCommitmentResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req resolveReq
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(req.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := d.Store.ResolveCommitment(r.Context(), id, anamnesia.CommitmentStatus(req.Status)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 5.6: MCP tools**

In `internal/mcp/server.go`:

```go
s.AddTool(
	mcp.NewTool("anamnesia_commitments_record",
		mcp.WithDescription("Record an open commitment (something owed by/to the user). Status defaults to 'open'."),
		mcp.WithString("body", mcp.Required()),
		mcp.WithString("owner", mcp.Description("party owing — default 'user'")),
		mcp.WithString("beneficiary", mcp.Description("party owed — default 'user'")),
		mcp.WithString("due_at", mcp.Description("RFC3339; optional")),
		mcp.WithString("user"), mcp.WithString("project"),
	),
	d.commitmentRecord,
)
s.AddTool(
	mcp.NewTool("anamnesia_commitments_list",
		mcp.WithDescription("List commitments. Default sort: open first, then by due date."),
		mcp.WithString("status", mcp.Description("open|done|dropped — default any")),
		mcp.WithNumber("limit"),
		mcp.WithString("user"), mcp.WithString("project"),
	),
	d.commitmentList,
)
s.AddTool(
	mcp.NewTool("anamnesia_commitments_resolve",
		mcp.WithDescription("Mark a commitment done or dropped."),
		mcp.WithString("id", mcp.Required()),
		mcp.WithString("status", mcp.Required(), mcp.Description("done|dropped")),
	),
	d.commitmentResolve,
)

func (d Deps) commitmentRecord(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	c := &anamnesia.Commitment{
		Scope: scope, Owner: argString(args, "owner"),
		Beneficiary: argString(args, "beneficiary"), Body: argString(args, "body"),
	}
	if s := argString(args, "due_at"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return bad(fmt.Errorf("due_at: %w", err))
		}
		c.DueAt = &t
	}
	if err := d.Store.RecordCommitment(ctx, c); err != nil {
		return bad(err)
	}
	return ok(c)
}

func (d Deps) commitmentList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	out, err := d.Store.ListCommitments(ctx, scope,
		anamnesia.CommitmentStatus(argString(args, "status")), argInt(args, "limit", 50))
	if err != nil {
		return bad(err)
	}
	return ok(out)
}

func (d Deps) commitmentResolve(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	id, err := argUUID(args, "id")
	if err != nil {
		return bad(err)
	}
	st := anamnesia.CommitmentStatus(argString(args, "status"))
	if err := d.Store.ResolveCommitment(ctx, id, st); err != nil {
		return bad(err)
	}
	return ok(map[string]any{"id": id, "status": st})
}
```

- [ ] **Step 5.7: Build, verify, commit**

```bash
go build ./...
./bin/anamnesia up
curl -sS -X POST http://localhost:8181/v1/commitments \
  -H 'Content-Type: application/json' \
  -d '{"user":"default","body":"send pricing doc to Sarah","beneficiary":"Sarah"}'
curl -sS 'http://localhost:8181/v1/commitments?user=default&status=open'
# Pick id from list; resolve:
curl -sS -X POST http://localhost:8181/v1/commitments/resolve \
  -H 'Content-Type: application/json' \
  -d '{"id":"<uuid>","status":"done"}'

git add internal/store/migrations/0007_commitments.sql pkg/anamnesia/commitments.go internal/store/commitments.go internal/httpapi/server.go internal/mcp/server.go
git commit -m "feat(substrate): commitments domain — open-loop ledger, multi-agent coherent"
```

---

## Task 6: Audit-by-subject

**Files:**
- Modify: `internal/store/working.go` (where `AuditTail` lives)
- Modify: `internal/httpapi/server.go`
- Modify: `internal/mcp/server.go`

- [ ] **Step 6.1: Add AuditForSubject**

In `internal/store/working.go`, append after `AuditTail`:

```go
// AuditForSubject returns audit_log rows whose target/target_id match,
// newest first. `kind` is e.g. "fact" | "experience" | "entity" |
// "commitment"; `id` is the row's primary key.
func (s *Store) AuditForSubject(ctx context.Context, kind string, id uuid.UUID, limit int) ([]*anamnesia.AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, at, user_id, project_id, op, target, target_id, actor, payload
		FROM audit_log
		WHERE target = $1 AND target_id = $2
		ORDER BY at DESC
		LIMIT $3`, kind, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.AuditEntry
	for rows.Next() {
		var (
			a       anamnesia.AuditEntry
			project *uuid.UUID
			user    *uuid.UUID
			tgtID   *uuid.UUID
			payload []byte
		)
		if err := rows.Scan(&a.ID, &a.At, &user, &project, &a.Op, &a.Target, &tgtID, &a.Actor, &payload); err != nil {
			return nil, err
		}
		a.UserID = user
		a.ProjectID = project
		a.TargetID = tgtID
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &a.Payload)
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}
```

Add `"encoding/json"` to the file imports if it isn't already there.

- [ ] **Step 6.2: HTTP route**

In `internal/httpapi/server.go`:

```go
mux.Handle("/v1/audit", d.protect(http.HandlerFunc(d.handleAudit)))

func (d Deps) handleAudit(w http.ResponseWriter, r *http.Request) {
	subject := r.URL.Query().Get("subject")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	if subject != "" {
		parts := strings.SplitN(subject, ":", 2)
		if len(parts) != 2 {
			http.Error(w, `subject must be "<kind>:<uuid>"`, http.StatusBadRequest)
			return
		}
		id, err := uuid.Parse(parts[1])
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out, err := d.Store.AuditForSubject(r.Context(), parts[0], id, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	ev := HookEvent{User: r.URL.Query().Get("user"), Project: r.URL.Query().Get("project")}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out, err := d.Store.AuditTail(r.Context(), scope, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
```

- [ ] **Step 6.3: Extend MCP tool**

In `internal/mcp/server.go`, modify the existing `anamnesia_audit` registration and handler:

```go
s.AddTool(
	mcp.NewTool("anamnesia_audit",
		mcp.WithDescription("Audit log: tail (default) or per-subject history if subject is provided."),
		mcp.WithString("subject", mcp.Description(`"<kind>:<uuid>" e.g. "fact:abc..."; if set, returns rows for that subject only`)),
		mcp.WithString("user"), mcp.WithString("project"),
		mcp.WithNumber("limit"),
	),
	d.audit,
)

func (d Deps) audit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	limit := argInt(args, "limit", 50)
	if subject := argString(args, "subject"); subject != "" {
		parts := strings.SplitN(subject, ":", 2)
		if len(parts) != 2 {
			return bad(errors.New(`subject must be "<kind>:<uuid>"`))
		}
		id, err := uuid.Parse(parts[1])
		if err != nil {
			return bad(err)
		}
		out, err := d.Store.AuditForSubject(ctx, parts[0], id, limit)
		if err != nil {
			return bad(err)
		}
		return ok(out)
	}
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	out, err := d.Store.AuditTail(ctx, scope, limit)
	if err != nil {
		return bad(err)
	}
	return ok(out)
}
```

Add `"strings"` and `"github.com/google/uuid"` to the imports if not already present (uuid likely is; strings may need adding).

- [ ] **Step 6.4: Build, verify, commit**

```bash
go build ./...
# Pick any fact id from earlier work, then:
curl -sS 'http://localhost:8181/v1/audit?subject=fact:<some-uuid>&limit=10'
git add internal/store/working.go internal/httpapi/server.go internal/mcp/server.go
git commit -m "feat(substrate): audit by subject — per-fact/experience/commitment provenance history"
```

---

## Task 7: Dock-on reference agent

**Files:**
- Create: `examples/agents/cli-companion/go.mod`
- Create: `examples/agents/cli-companion/main.go`
- Create: `examples/agents/cli-companion/README.md`

This agent must consume **only** the public MCP surface (no `internal/...` imports). It's the proof that an external agent can adopt persona, search, record commitments, and ingest.

- [ ] **Step 7.1: Create the module**

```bash
mkdir -p examples/agents/cli-companion
cd examples/agents/cli-companion
go mod init github.com/flohs/anamnesia-open-source/examples/agents/cli-companion
go get github.com/mark3labs/mcp-go@latest
go get github.com/anthropics/anthropic-sdk-go@latest
```

If the `mark3labs/mcp-go` client API differs from what's shown below (the package's surface has shifted between versions), adjust the `client.NewStreamableHttpClient` / `CallTool` calls to match the version pulled in.

- [ ] **Step 7.2: Write the agent**

Create `examples/agents/cli-companion/main.go`:

```go
// cli-companion is the reference dock-on agent for Anamnesia. It boots
// from the user's identity, accepts prompts, searches memory, optionally
// records commitments, and ingests the transcript on exit.
//
// Calls Anamnesia exclusively through the public MCP surface at
// http://localhost:8181/mcp; MUST NOT import any internal/* package.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

var (
	mcpURL    = flag.String("mcp", "http://localhost:8181/mcp", "Anamnesia MCP endpoint")
	user      = flag.String("user", "default", "user handle")
	project   = flag.String("project", "", "project slug (optional)")
	modelName = flag.String("model", "claude-sonnet-4-6", "Anthropic model")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	mc, err := client.NewStreamableHttpClient(*mcpURL)
	if err != nil {
		return fmt.Errorf("mcp client: %w", err)
	}
	if _, err := mc.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		return fmt.Errorf("mcp init: %w", err)
	}

	persona := callIdentity(ctx, mc, *user, *project)
	caps := callCapabilities(ctx, mc, *user, *project)
	log.Printf("dock-on agent ready: %d capabilities available", caps)

	anth := anthropic.NewClient(option.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")))

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	var transcript strings.Builder

	fmt.Println("# cli-companion — type a prompt; ^D to exit and ingest")
	for {
		fmt.Print("you> ")
		if !scanner.Scan() {
			break
		}
		prompt := scanner.Text()
		if strings.TrimSpace(prompt) == "" {
			continue
		}

		hits := callSearch(ctx, mc, *user, *project, prompt)

		sysParts := []string{}
		if persona != "" {
			sysParts = append(sysParts, persona)
		}
		if hits != "" {
			sysParts = append(sysParts, "Anamnesia context:\n"+hits)
		}
		msg, err := anth.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model(*modelName),
			MaxTokens: 1024,
			System:    []anthropic.TextBlockParam{{Text: strings.Join(sysParts, "\n\n")}},
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
			},
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "claude:", err)
			continue
		}
		reply := ""
		for _, b := range msg.Content {
			if b.Type == "text" {
				reply += b.Text
			}
		}
		fmt.Printf("ai> %s\n", reply)

		if hasCommitmentLanguage(reply) {
			callRecordCommitment(ctx, mc, *user, *project, firstSentence(reply))
		}

		transcript.WriteString("user: " + prompt + "\nai: " + reply + "\n\n")
	}

	if transcript.Len() > 0 {
		callIngest(ctx, mc, *user, *project, transcript.String())
		log.Println("transcript ingested")
	}
	return nil
}

func callTool(ctx context.Context, mc *client.Client, name string, args map[string]any) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := mc.CallTool(rctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name, Arguments: args},
	})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("tool %s: %v", name, res.Content)
	}
	for _, c := range res.Content {
		if c.Type == "text" {
			return c.Text, nil
		}
	}
	return "", nil
}

func callIdentity(ctx context.Context, mc *client.Client, user, project string) string {
	raw, err := callTool(ctx, mc, "anamnesia_identity", map[string]any{"user": user, "project": project})
	if err != nil {
		log.Printf("identity: %v", err)
		return ""
	}
	var id struct {
		SystemPrompt string `json:"system_prompt"`
	}
	if err := json.Unmarshal([]byte(raw), &id); err != nil {
		return ""
	}
	return id.SystemPrompt
}

func callCapabilities(ctx context.Context, mc *client.Client, user, project string) int {
	raw, err := callTool(ctx, mc, "anamnesia_capabilities", map[string]any{"user": user, "project": project})
	if err != nil {
		return 0
	}
	var arr []any
	_ = json.Unmarshal([]byte(raw), &arr)
	return len(arr)
}

func callSearch(ctx context.Context, mc *client.Client, user, project, text string) string {
	raw, _ := callTool(ctx, mc, "anamnesia_search", map[string]any{
		"user": user, "project": project, "text": text, "k": 5,
	})
	return raw
}

func callRecordCommitment(ctx context.Context, mc *client.Client, user, project, body string) {
	_, _ = callTool(ctx, mc, "anamnesia_commitments_record", map[string]any{
		"user": user, "project": project, "body": body,
	})
}

func callIngest(ctx context.Context, mc *client.Client, user, project, content string) {
	_, _ = callTool(ctx, mc, "anamnesia_ingest", map[string]any{
		"user": user, "project": project, "kind": "cli-companion-session", "content": content,
	})
}

var commitmentMarkers = []string{
	"I'll ", "I will ", "I'll send", "I'll follow up", "Let me ", "I promise",
}

func hasCommitmentLanguage(s string) bool {
	for _, m := range commitmentMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func firstSentence(s string) string {
	for _, sep := range []string{". ", ".\n"} {
		if i := strings.Index(s, sep); i > 0 {
			return s[:i+1]
		}
	}
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
```

- [ ] **Step 7.3: README**

Create `examples/agents/cli-companion/README.md`:

```markdown
# cli-companion — reference dock-on agent

A minimal Go CLI that demonstrates how an external agent docks onto
Anamnesia through the public MCP surface only.

## Boot
- Calls `anamnesia_identity` to load the user's persona as system prompt.
- Calls `anamnesia_capabilities` to discover registered skills.

## Per turn
- Calls `anamnesia_search` for context.
- Sends prompt + persona + context to Claude via the Anthropic SDK.
- If the reply contains future-tense commitment language, records it
  via `anamnesia_commitments_record`.

## On exit
- Ingests the whole transcript via `anamnesia_ingest` so the extractor
  decides what's worth keeping.

## Constraint
This module deliberately does **not** import any
`github.com/flohs/anamnesia-open-source/internal/...` package. The MCP
surface is the only contract.

## Run

    export ANTHROPIC_API_KEY=sk-ant-...
    go run . -user=default
```

- [ ] **Step 7.4: Verify the internal-import constraint**

```bash
cd examples/agents/cli-companion
go build ./...
go list -deps ./... | grep '^github.com/flohs/anamnesia-open-source/internal/'
# Expected: NO output. Any line printed = constraint broken — fix.
```

- [ ] **Step 7.5: End-to-end smoke**

```bash
cd ../../..
./bin/anamnesia up
# (in another terminal)
cd examples/agents/cli-companion
go run . -user=default
# Type: "Remind me to email the design doc to Sarah"
# Expected: reply printed; commitment recorded; ^D ingests transcript.
# Verify:
curl -sS 'http://localhost:8181/v1/commitments?user=default&status=open'
```

- [ ] **Step 7.6: Commit**

```bash
git add examples/agents/cli-companion/
git commit -m "feat(substrate): cli-companion — reference dock-on agent using only public MCP surface"
```

---

## Final verification

Run the four success criteria from the spec:

- [ ] `curl -sS 'http://localhost:8181/v1/identity?user=default'` returns non-empty `system_prompt` when a persona fact exists.
- [ ] `curl -sS -X POST http://localhost:8181/v1/briefing -d '{"user":"default","since":"2026-05-20T00:00:00Z"}'` returns `summary` and `adjacent`.
- [ ] `cli-companion` runs without importing `internal/...` (already verified in Step 7.4).
- [ ] `anamnesia_audit(subject="fact:<id>")` returns chronological rows for a fact.

If any check fails, the corresponding task is incomplete — re-open and fix.
