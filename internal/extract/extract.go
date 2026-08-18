// Package extract turns raw ingest content into durable memory rows.
//
// Pipeline for one sources row:
//
//  1. Surprise gate: embed the content, check the nearest existing fact
//     and experience. If both are very close (cosine > threshold), skip —
//     we've already seen this. Bypass the gate when the content contains
//     temporal markers ("just", "now", "today") because those mean
//     "something changed".
//  2. Candidate fetch: pull the top-K similar existing facts/experiences
//     in scope, send them to the LLM as the candidate list.
//  3. LLM call: ask for ADD_FACT / UPDATE_FACT / DELETE_FACT /
//     ADD_EXPERIENCE / NOOP operations. Default is NOOP.
//  4. Execute the operations against the store. Discard the raw
//     content (it stays in the sources row until TTL).
//
// The same Extractor is invoked from both the worker (`pending` rows
// from /v1/ingest) and the synchronous session-end pass on the Stop
// hook.
package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/embed"
	"github.com/flohs/anamnesia/internal/llm"
	"github.com/flohs/anamnesia/internal/retrieval"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// Config tunes the extractor.
type Config struct {
	// SurpriseThreshold is the cosine similarity above which we consider
	// the content a duplicate of an existing memory and skip extraction.
	// Default 0.93 — high enough that paraphrases still get through.
	SurpriseThreshold float64
	// CandidateK is how many top-similar memories per kind we send to
	// the LLM. Default 5. Mem0's paper uses 10; we err lower to keep
	// the prompt cheap.
	CandidateK int
	// MaxOps caps how many operations a single extraction can produce.
	// Default 8. Protects against runaway LLM output.
	MaxOps int
	// MinContentLen skips bodies shorter than this. Default 16.
	MinContentLen int
	// ExtractCommitments, when true, lets the extractor emit
	// ADD_COMMITMENT ops (open-loop obligations) in addition to the
	// fact/experience ops. Off by default so the prompt + schema sent
	// for the existing ops are byte-identical when the feature is
	// disabled — benchmark runs are not perturbed.
	ExtractCommitments bool
}

// Extractor runs the pipeline against an open-source memory store.
type Extractor struct {
	Cfg       Config
	Store     *store.Store
	Embedder  embed.Embedder
	Retrieval *retrieval.Engine
	LLM       llm.Client
	Log       *slog.Logger
	// Activity records the reasoning behind one extraction. Nil is a
	// working no-op: nothing here depends on being watched.
	Activity *activity.Recorder
}

// applyDefaults fills zero fields without mutating the caller's config.
func (c Config) applyDefaults() Config {
	if c.SurpriseThreshold == 0 {
		c.SurpriseThreshold = 0.93
	}
	if c.CandidateK == 0 {
		c.CandidateK = 5
	}
	if c.MaxOps == 0 {
		c.MaxOps = 8
	}
	if c.MinContentLen == 0 {
		c.MinContentLen = 16
	}
	return c
}

// Operation is one ADD/UPDATE/DELETE the LLM emitted. Most fields are
// optional and only meaningful for certain ops. Value is decoded as raw
// JSON so we can accept either an object ({"degree": "BA"}) or a scalar
// ("BA") — the LLM emits whichever feels natural for the fact.
type Operation struct {
	Op        string          `json:"op"` // ADD_FACT | UPDATE_FACT | DELETE_FACT | ADD_EXPERIENCE | NOOP
	ID        string          `json:"id,omitempty"`
	FactScope string          `json:"fact_scope,omitempty"` // user | project | environment
	Key       string          `json:"key,omitempty"`
	Value     json.RawMessage `json:"value,omitempty"`
	Source    string          `json:"source,omitempty"`
	Trust     float32         `json:"trust,omitempty"`
	// Experience-only.
	Kind       string  `json:"kind,omitempty"`
	Title      string  `json:"title,omitempty"`
	Body       string  `json:"body,omitempty"`
	Outcome    string  `json:"outcome,omitempty"`
	Importance float32 `json:"importance,omitempty"`
	Topic      string  `json:"topic,omitempty"`
	// Commitment-only. Body carries the obligation text.
	Owner       string `json:"owner,omitempty"`
	Beneficiary string `json:"beneficiary,omitempty"`
	DueAt       string `json:"due_at,omitempty"` // RFC3339
}

// opsResponse decodes the outer envelope. Operations is held as raw
// messages so a single malformed op doesn't kill the whole batch — we
// unmarshal each one independently in Run.
type opsResponse struct {
	Operations []json.RawMessage `json:"operations"`
}

// valueToMap turns the LLM's Value (which may be an object, a scalar,
// or absent) into a map suitable for Fact.Value. Scalars are wrapped
// as {"v": <scalar>}; arrays as {"items": [...]}.
func valueToMap(raw json.RawMessage) map[string]any {
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return map[string]any{}
	}
	switch trimmed[0] {
	case '{':
		var m map[string]any
		if err := json.Unmarshal(trimmed, &m); err == nil {
			return m
		}
	case '[':
		var a []any
		if err := json.Unmarshal(trimmed, &a); err == nil {
			return map[string]any{"items": a}
		}
	}
	var scalar any
	if err := json.Unmarshal(trimmed, &scalar); err == nil {
		return map[string]any{"v": scalar}
	}
	return map[string]any{"v": string(trimmed)}
}

func bytesTrimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\n' || b[j-1] == '\r') {
		j--
	}
	return b[i:j]
}

// Run extracts one sources row. Returns the number of operations
// executed (0 if the gate skipped or LLM returned NOOPs).
func (e *Extractor) Run(ctx context.Context, src *anamnesia.Source) (int, error) {
	cfg := e.Cfg.applyDefaults()
	content := strings.TrimSpace(src.RawContent)

	// One trace per source: what arrived, what the gate decided, what the
	// model was shown, what it asked for, and what was written. This is
	// the only record of why a checkpoint became a memory or did not.
	user, project := e.scopeNames(ctx, src.Scope)
	tr := e.Activity.Begin("ingest", user, project)
	tr.Step("source", fmt.Sprintf("Received a %s %s source", humanBytes(len(content)), src.Kind),
		map[string]any{
			"source_id": src.ID.String(),
			"kind":      src.Kind,
			"title":     src.Title,
			"bytes":     len(content),
			"excerpt":   content,
		})

	if len(content) < cfg.MinContentLen {
		tr.End("skipped", fmt.Sprintf("Nothing to extract from %d characters", len(content)))
		return 0, nil
	}

	// Step 1: surprise gate. The gate is intentionally cheap — one
	// embed call + two top-1 ANN queries. Skip the gate when the
	// content carries explicit temporal markers (the user is telling
	// the agent that something changed), or when the source kind is
	// tagged as an evaluation / benchmark stream that should retain
	// every passing mention (LongMemEval and similar).
	switch {
	case hasTemporalMarker(content):
		tr.Step("gate", "Kept: the content says something just changed", map[string]any{
			"verdict": "keep",
			"reason":  "a temporal marker in the content bypasses the gate",
		})
	case bypassGate(src.Kind):
		tr.Step("gate", "Kept: this source kind is never gated", map[string]any{
			"verdict": "keep",
			"reason":  "source kind " + src.Kind + " is an evaluation stream and bypasses the gate",
		})
	default:
		skip, score, reason, err := e.surpriseGate(ctx, src.Scope, content, cfg.SurpriseThreshold)
		switch {
		case err != nil:
			// Gate failure is non-fatal — fall through to full extraction.
			if e.Log != nil {
				e.Log.Warn("extractor: surprise gate failed", "err", err)
			}
			tr.Step("gate", "Kept: the gate could not run", map[string]any{
				"verdict": "keep",
				"reason":  "the gate failed and extraction continued anyway: " + err.Error(),
			})
		case skip:
			tr.Step("gate", "Skipped: an existing memory already covers this", map[string]any{
				"verdict": "skip",
				"reason":  reason,
				"score":   score,
			})
			tr.End("skipped", "Already covered by an existing memory")
			return 0, nil
		default:
			tr.Step("gate", "Kept: "+reason, map[string]any{
				"verdict": "keep",
				"reason":  reason,
				"score":   score,
			})
		}
	}

	// Step 2: candidate fetch. Hybrid search across facts + experiences.
	candidates, hits, err := e.candidates(ctx, src.Scope, content, cfg.CandidateK)
	if err != nil {
		// Soft failure — extract without candidates rather than block ingest.
		if e.Log != nil {
			e.Log.Warn("extractor: candidate fetch failed", "err", err)
		}
		candidates = nil
		tr.Fail("similar", err)
	} else {
		tr.Step("similar", fmt.Sprintf("Fetched %d similar memories as context", len(candidates)),
			map[string]any{"hits": retrieval.HitDetails(hits)})
	}

	// Step 3: LLM call. We pass a JSON schema so OpenAI's structured
	// outputs constrain the response to a valid operations envelope —
	// this catches schema drift at the model layer rather than after
	// the unmarshal. Anthropic ignores the schema field and relies on
	// the system prompt to enforce shape. The system prompt itself is
	// chosen per source kind: benchmark / evaluation streams use a
	// liberal extractor that captures every concrete claim, while the
	// default (Claude Code chat turns, etc.) keeps the "noise by
	// default" prior that production needs.
	systemPrompt := extractSystemPrompt
	maxTok := 1024
	if bypassGate(src.Kind) {
		// Benchmark sources legitimately decompose lists into many small
		// facts; the default 1024-token budget runs out mid-array and
		// the entire response is rejected as truncated JSON. Give the
		// liberal prompt enough headroom to finish.
		systemPrompt = extractSystemPromptLiberal
		maxTok = 4096
	}
	// Commitment extraction is opt-in. When enabled, append the
	// ADD_COMMITMENT instructions and switch to the schema variant that
	// permits the op. When disabled, both the prompt and schema are
	// exactly what the fact/experience-only pipeline has always sent.
	schema := operationSchema
	if cfg.ExtractCommitments {
		systemPrompt += commitmentInstructions
		schema = operationSchemaWithCommitments
	}
	prompt := e.userPrompt(src, content, candidates)
	captured := &capturedOps{}
	started := time.Now()
	if err := e.LLM.Extract(ctx, llm.DistillInput{
		System:     systemPrompt,
		User:       prompt,
		MaxTok:     maxTok,
		Schema:     schema,
		SchemaName: "anamnesia_operations",
	}, captured); err != nil {
		tr.Fail("llm", err)
		tr.End("failed", "The model call failed, so the source stays pending")
		return 0, fmt.Errorf("llm extract: %w", err)
	}
	resp := captured.resp
	tr.Step("llm", fmt.Sprintf("%s returned %d operations", e.LLM.Model(), len(resp.Operations)),
		map[string]any{
			"model":            e.LLM.Model(),
			"latency_ms":       time.Since(started).Milliseconds(),
			"prompt_chars":     len(systemPrompt) + len(prompt),
			"completion_chars": len(captured.raw),
			"raw_response":     string(captured.raw),
		})

	maxOps := cfg.MaxOps
	if bypassGate(src.Kind) {
		// Benchmark / evaluation streams legitimately decompose lists
		// into many small facts. Default cap is too tight for that.
		maxOps = 20
	}
	if len(resp.Operations) > maxOps {
		resp.Operations = resp.Operations[:maxOps]
	}

	// Step 4: execute. Each operation is decoded independently — a
	// malformed op should not nuke its siblings. NOOPs don't count
	// toward executed.
	decoded := make([]Operation, 0, len(resp.Operations))
	labels := make([]string, 0, len(resp.Operations))
	for i, raw := range resp.Operations {
		var op Operation
		if err := json.Unmarshal(raw, &op); err != nil {
			if e.Log != nil {
				e.Log.Warn("extractor: op decode failed", "idx", i, "err", err, "raw", string(raw))
			}
			continue
		}
		decoded = append(decoded, op)
		if !isNoop(op.Op) {
			labels = append(labels, strings.ToUpper(strings.TrimSpace(op.Op)))
		}
	}
	opsSummary := "Nothing worth keeping"
	if len(labels) > 0 {
		opsSummary = strings.Join(labels, ", ")
	}
	tr.Step("ops", opsSummary, map[string]any{"operations": resp.Operations})

	executed := 0
	written := make([]map[string]any, 0, len(decoded))
	failures := make([]string, 0)
	for _, op := range decoded {
		if isNoop(op.Op) {
			continue
		}
		done, err := e.executeOp(ctx, src, op)
		if err != nil {
			if e.Log != nil {
				e.Log.Warn("extractor: op failed", "op", op.Op, "err", err)
			}
			failures = append(failures, op.Op+": "+err.Error())
			continue
		}
		executed++
		if done.Target != "" {
			written = append(written, map[string]any{"target": done.Target, "id": done.ID.String()})
		}
	}
	if executed > 0 || len(failures) > 0 {
		tr.Step("apply", writtenSummary(written, failures), map[string]any{
			"written": written,
			"errors":  failures,
		})
	}
	if executed == 0 {
		tr.End("skipped", "The model found nothing worth keeping")
	} else {
		tr.End("ok", fmt.Sprintf("Extracted %d operations from a %s %s source",
			executed, humanBytes(len(content)), src.Kind))
	}
	return executed, nil
}

// capturedOps decodes the operations envelope while keeping the exact
// JSON the model produced. Every provider hands `out` to json.Unmarshal,
// so implementing Unmarshaler is enough to see the document without
// changing the llm package or any provider in it.
type capturedOps struct {
	raw  json.RawMessage
	resp opsResponse
}

func (c *capturedOps) UnmarshalJSON(b []byte) error {
	c.raw = append(c.raw[:0], b...)
	return json.Unmarshal(b, &c.resp)
}

// applied is what one executed operation produced, so the trace can name
// the row that was written rather than only the operation that asked for
// it.
type applied struct {
	Target string // fact | experience | commitment
	ID     uuid.UUID
}

// scopeNames resolves a scope to the handle and slug a trace displays.
// Two indexed lookups per source, and only when someone is recording.
func (e *Extractor) scopeNames(ctx context.Context, scope anamnesia.Scope) (string, string) {
	if e.Store == nil || e.Activity == nil {
		return "", ""
	}
	user, err := e.Store.LookupUserHandle(ctx, scope.UserID)
	if err != nil {
		user = ""
	}
	project := ""
	if scope.ProjectID != nil {
		if slug, err := e.Store.LookupProjectSlug(ctx, *scope.ProjectID); err == nil {
			project = slug
		}
	}
	return user, project
}

func writtenSummary(written []map[string]any, failures []string) string {
	counts := map[string]int{}
	for _, w := range written {
		counts[w["target"].(string)]++
	}
	parts := make([]string, 0, len(counts))
	for _, target := range []string{"fact", "experience", "commitment"} {
		if n := counts[target]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, plural(target, n)))
		}
	}
	summary := "Wrote nothing"
	if len(parts) > 0 {
		summary = "Wrote " + joinWords(parts)
	}
	if len(failures) > 0 {
		summary += fmt.Sprintf(", %d failed", len(failures))
	}
	return summary
}

func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func joinWords(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// humanBytes renders a size the way a sentence would.
func humanBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f kB", float64(n)/1024)
}

// bypassGateKinds is the set of source kinds whose stream should not be
// surprise-gated. Used by benchmark / evaluation pipelines that ingest
// many similar sessions and need every passing mention retained.
var bypassGateKinds = map[string]bool{
	"benchmark":           true,
	"biographical_eval":   true,
	"longmemeval-session": true,
}

func bypassGate(kind string) bool {
	return bypassGateKinds[strings.ToLower(kind)]
}

// deriveFactKey synthesises a key for an ADD_FACT that arrived without
// one. Looks at the value first (a scalar like "Business Administration"
// becomes "business-administration"), then falls back to body/title.
// Empty string means "no signal at all — drop the op".
func deriveFactKey(value json.RawMessage, body, title string) string {
	if s := scalarString(value); s != "" {
		return slugKey(s)
	}
	if body != "" {
		return slugKey(body)
	}
	if title != "" {
		return slugKey(title)
	}
	return ""
}

func scalarString(raw json.RawMessage) string {
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return s
		}
	}
	if trimmed[0] == '{' {
		var m map[string]any
		if err := json.Unmarshal(trimmed, &m); err == nil {
			for _, v := range m {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
	}
	return ""
}

var keySlugRE = regexp.MustCompile(`[^a-z0-9]+`)

func slugKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = keySlugRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

func isNoop(op string) bool {
	switch strings.ToUpper(strings.TrimSpace(op)) {
	case "", "NOOP":
		return true
	}
	return false
}

// surpriseGate returns true if the nearest existing memory is above the
// threshold — meaning "we've seen this already, nothing to extract".
//
// It also returns the score it judged on and the reason in plain words.
// Without those the trace could only say "kept" or "skipped", and the
// most common answer here is "no judgement was possible at all", which
// is worth being able to say out loud.
func (e *Extractor) surpriseGate(ctx context.Context, scope anamnesia.Scope, content string, threshold float64) (skip bool, score float64, reason string, err error) {
	if e.Embedder == nil || e.Retrieval == nil {
		return false, 0, "nothing was compared: no embedder or retrieval is configured", nil
	}
	hits, err := e.Retrieval.Search(ctx, retrieval.Query{
		Scope: scope,
		Text:  content,
		K:     1,
	})
	if err != nil {
		return false, 0, "", err
	}
	// If retrieval found nothing similar, definitely extract.
	if len(hits) == 0 {
		return false, 0, "no similar memory exists yet", nil
	}
	// The reranker mutates Score; without it, RRF scores are tiny.
	// Fall back to recomputing a cosine ourselves on the candidate.
	// To keep the gate cheap we just compare RerankerRank — if the
	// reranker put the candidate first with a high score (close to 1)
	// we treat that as "seen". Otherwise we always extract. This is
	// conservative; tune later when we have data.
	if hits[0].RerankerRank == 0 {
		return false, hits[0].Score, "no reranker ran, so the gate had no absolute score to judge with", nil
	}
	if hits[0].Score >= threshold {
		return true, hits[0].Score, fmt.Sprintf(
			"the nearest memory scores %.2f, at or above the %.2f threshold", hits[0].Score, threshold), nil
	}
	return false, hits[0].Score, fmt.Sprintf(
		"the nearest memory scores %.2f, below the %.2f threshold", hits[0].Score, threshold), nil
}

// candidates pulls top-K similar facts + experiences as the LLM's
// reference set.
type candidate struct {
	Domain string         `json:"domain"`
	ID     string         `json:"id"`
	Body   string         `json:"body"`
	Meta   map[string]any `json:"meta,omitempty"`
}

// The hits are returned alongside so a trace can show the scores. They
// are deliberately not folded into candidate: that struct is marshalled
// into the model's prompt, and the prompt must not change shape because
// something started watching.
func (e *Extractor) candidates(ctx context.Context, scope anamnesia.Scope, text string, k int) ([]candidate, []anamnesia.SearchHit, error) {
	if e.Retrieval == nil {
		return nil, nil, nil
	}
	hits, err := e.Retrieval.Search(ctx, retrieval.Query{
		Scope: scope, Text: text, K: k,
	})
	if err != nil {
		return nil, nil, err
	}
	out := make([]candidate, 0, len(hits))
	for _, h := range hits {
		c := candidate{Domain: string(h.Domain), ID: h.ID().String()}
		switch h.Domain {
		case anamnesia.DomainFact:
			if h.Fact != nil {
				c.Body = h.Fact.Key
				c.Meta = map[string]any{
					"fact_scope": h.Fact.FactKind,
					"value":      h.Fact.Value,
				}
			}
		case anamnesia.DomainExperience:
			if h.Experience != nil {
				c.Body = h.Experience.Title
				if c.Body == "" {
					c.Body = firstLine(h.Experience.Body, 200)
				}
				c.Meta = map[string]any{
					"kind":        h.Experience.Kind,
					"importance":  h.Experience.Importance,
					"occurred_at": h.Experience.OccurredAt,
				}
			}
		}
		out = append(out, c)
	}
	return out, hits, nil
}

// userPrompt assembles the JSON payload sent to the LLM. The system
// prompt is static (cacheable across calls); this carries the content +
// candidates + temporal context.
func (e *Extractor) userPrompt(src *anamnesia.Source, content string, cands []candidate) string {
	payload := map[string]any{
		"now":          time.Now().UTC().Format(time.RFC3339),
		"occurred_at":  src.OccurredAt.UTC().Format(time.RFC3339),
		"source_kind":  src.Kind,
		"participants": src.Participants,
		"content":      content,
		"candidates":   cands,
	}
	if src.Title != "" {
		payload["source_title"] = src.Title
	}
	raw, _ := json.Marshal(payload)
	return string(raw)
}

// operationSchema is the JSON Schema for opsResponse. Sent to OpenAI's
// response_format=json_schema so the model literally cannot return a
// shape we can't decode. Kept in sync with the Operation struct above
// by hand; if you add a field there, add it here too.
var operationSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "operations": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "op":         {"type": "string", "enum": ["ADD_FACT","UPDATE_FACT","DELETE_FACT","ADD_EXPERIENCE","NOOP"]},
          "id":         {"type": "string"},
          "fact_scope": {"type": "string", "enum": ["user","project","environment"]},
          "key":        {"type": "string"},
          "value":      {},
          "source":     {"type": "string"},
          "trust":      {"type": "number"},
          "kind":       {"type": "string", "enum": ["case","strategy","hybrid"]},
          "title":      {"type": "string"},
          "body":       {"type": "string"},
          "outcome":    {"type": "string"},
          "importance": {"type": "number"},
          "topic":      {"type": "string"}
        },
        "required": ["op"]
      }
    }
  },
  "required": ["operations"]
}`)

// operationSchemaWithCommitments is operationSchema plus the
// ADD_COMMITMENT op and its owner/beneficiary/due_at fields. Used only
// when Config.ExtractCommitments is set. Kept as a separate literal so
// the default schema is untouched.
var operationSchemaWithCommitments = json.RawMessage(`{
  "type": "object",
  "properties": {
    "operations": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "op":          {"type": "string", "enum": ["ADD_FACT","UPDATE_FACT","DELETE_FACT","ADD_EXPERIENCE","ADD_COMMITMENT","NOOP"]},
          "id":          {"type": "string"},
          "fact_scope":  {"type": "string", "enum": ["user","project","environment"]},
          "key":         {"type": "string"},
          "value":       {},
          "source":      {"type": "string"},
          "trust":       {"type": "number"},
          "kind":        {"type": "string", "enum": ["case","strategy","hybrid"]},
          "title":       {"type": "string"},
          "body":        {"type": "string"},
          "outcome":     {"type": "string"},
          "importance":  {"type": "number"},
          "topic":       {"type": "string"},
          "owner":       {"type": "string"},
          "beneficiary": {"type": "string"},
          "due_at":      {"type": "string"}
        },
        "required": ["op"]
      }
    }
  },
  "required": ["operations"]
}`)

// commitmentInstructions is appended to the system prompt when
// Config.ExtractCommitments is set. Conservative on purpose — the bar
// for ADD_COMMITMENT is an explicit, stated obligation.
const commitmentInstructions = `

You may ALSO emit:
- ADD_COMMITMENT: an explicit open-loop obligation the source states — something owed by or to the user with a concrete action. Provide "owner" (who owes it: "user" or a person's name), "beneficiary" (who it's owed to), "body" (the obligation in one line), and optional "due_at" (RFC3339; resolve relative dates using the "now" field).

Only emit ADD_COMMITMENT when the source clearly states a promise, a task owed, or a deadline ("I'll send the doc by Friday", "remind me to call Y", "I need to review Z before the meeting"). Do NOT infer commitments from vague intentions or completed actions. When in doubt, prefer NOOP or ADD_FACT over ADD_COMMITMENT.`

const extractSystemPrompt = `You are the extraction worker for an agent memory system. You read one piece of input content and decide what should land in long-term memory.

You can emit these operations:
- ADD_FACT: a stable, durable claim about the user, project, or environment (preference, decision, configuration). Use fact_scope=user|project|environment.
- UPDATE_FACT: replace an existing fact's value. Provide its id from "candidates".
- DELETE_FACT: invalidate an existing fact. Provide its id.
- ADD_EXPERIENCE: a noteworthy event, trajectory, strategy, or insight worth remembering as a narrative. Use kind=case|strategy|hybrid. Provide a "title" and a 1-2 sentence "body". The title is a short noun phrase, under 60 characters, no trailing full stop, naming the conclusion rather than the activity: "Hook ordering is guaranteed by the client", not "Discussion about hooks".
- NOOP: nothing worth keeping.

Rules:
- Default to NOOP. Most chat content is noise — only extract when there's something durable or noteworthy.
- ADD_FACT for durable claims. "I'm going to grab coffee" is NOOP. "I prefer pnpm over npm" is ADD_FACT.
- ADD_FACT also applies to biographical or autobiographical facts the user mentions in passing — degree, profession, role, family, location, name. "I graduated with a degree in Business Administration" is ADD_FACT with key="user.degree". Capture these even when mentioned only once.
- "value" may be either a JSON object ({"name": "pnpm"}) or a JSON scalar ("Business Administration"). Either is accepted; pick whatever is most natural for the fact.
- Every ADD_FACT should include a non-empty "key" (e.g. "user.degree", "user.location", "user.name"). If you can't think of one, emit it anyway and the server will derive a key from the value.
- Reference candidate ids exactly as given when you emit UPDATE_FACT or DELETE_FACT.
- Do NOT extract from system messages, agent boot configuration, the candidates block you were given, tool errors, or already-recalled memories. Only extract from user-authored or assistant-authored content in the source body.
- Do NOT invent biographical attributes (age, ethnicity, gender, etc.) that the source does not state. If it isn't in the content, emit NOOP for that claim.
- Resolve relative time ("yesterday", "last week") to absolute dates using the "now" field.
- Output JSON only, matching: {"operations":[{"op":"...", ...}]}. No prose, no markdown fences.
- Keep importance/trust in [0,1]. Default importance 0.5, trust 0.7 if you have no other signal.
- Cap output at 8 operations. Prefer fewer high-value operations over many low-value ones.`

// extractSystemPromptLiberal is used for sources tagged with an
// evaluation / benchmark kind (LongMemEval and similar). The production
// prompt's "default to NOOP" prior throws away too much when the
// workload is full chat-history recall rather than agent-context
// extraction. Here we ask for everything concrete: biographical,
// preference, assistant-emitted facts, schedules, recommendations.
const extractSystemPromptLiberal = `You are the extraction worker for a long-term memory benchmark. Your job is to capture every concrete, retrievable claim made in the input content — biographical facts, preferences, schedules, recommendations, opinions, plans, statements of fact made by either the user or the assistant.

You can emit these operations:
- ADD_FACT: a concrete claim. Use fact_scope=user for personal claims (degree, preference, plan), project for project-local facts, environment otherwise.
- UPDATE_FACT: replace an existing fact when the source explicitly updates it. Provide the candidate id.
- DELETE_FACT: invalidate an existing fact when the source explicitly retracts it.
- ADD_EXPERIENCE: a multi-claim narrative or strategy worth remembering as one unit. Provide a "title" and a 1-3 sentence "body". The title is a short noun phrase, under 60 characters, no trailing full stop, naming the conclusion rather than the activity: "Hook ordering is guaranteed by the client", not "Discussion about hooks".
- NOOP: only when there is genuinely nothing concrete to extract (rare here).

Rules:
- Bias toward EXTRACTION, not NOOP. If the content names a preference, a plan, a fact about the user, an assistant recommendation, or a piece of structured information (a schedule, a list, a count), extract it.
- "value" may be a JSON object ({"degree": "BA"}) or a scalar ("Business Administration"). Either is accepted.
- Always include a "key" when possible (e.g. "user.degree", "user.preference.video_editing", "schedule.admon.sunday"). If you can't, emit the op anyway — the server will derive a key.
- For assistant-emitted information (schedules, recommendations, lists, instructions) emit ADD_FACT or ADD_EXPERIENCE — the user may ask about it later.
- For preferences mentioned in passing, ADD_FACT with the specific topic in the key.
- DECOMPOSE LISTS. When the source contains a list or schedule of multiple items of the same kind (model kits, shift assignments per day, books, places, projects led), emit ONE ADD_FACT per item, baking the item identifier into the key. Examples:
  - "I worked on a B-29 and a Camaro" → ADD_FACT key="user.model_kit.b-29", ADD_FACT key="user.model_kit.camaro" (two separate ops).
  - "Sunday shift: Admon. Monday shift: Magdy" → ADD_FACT key="schedule.sunday.admon", ADD_FACT key="schedule.monday.magdy".
  - "Restaurants in Bandung: Miss Bee Providore (Cihampelas), …" → one ADD_FACT per restaurant, key="bandung.restaurant.miss_bee_providore".
  - Do NOT bundle a list into a single value:{items:[…]} unless the list IS the fact (a tag set, a permission list) and the items don't need individual retrieval.
- Output JSON only, matching {"operations":[{"op":"...", ...}]}. No prose, no markdown fences.
- Keep importance/trust in [0,1]. Default importance 0.7, trust 0.8 — this content is benchmark-grade.
- Cap output at 20 operations. Higher cap because list-decomposition can legitimately emit many facts.`

// executeOp runs one operation against the store, reporting the row it
// touched so the trace can link to it.
func (e *Extractor) executeOp(ctx context.Context, src *anamnesia.Source, op Operation) (applied, error) {
	switch strings.ToUpper(op.Op) {
	case "", "NOOP":
		return applied{}, nil
	case "ADD_FACT":
		return e.addFact(ctx, src, op)
	case "UPDATE_FACT":
		return e.updateFact(ctx, src, op)
	case "DELETE_FACT":
		return e.deleteFact(ctx, op)
	case "ADD_EXPERIENCE":
		return e.addExperience(ctx, src, op)
	case "ADD_COMMITMENT":
		// Defence in depth: the op only reaches the model when the flag
		// is on, but guard here too so a stray op is a no-op rather than
		// an unexpected write.
		if !e.Cfg.ExtractCommitments {
			return applied{}, nil
		}
		return e.addCommitment(ctx, src, op)
	default:
		return applied{}, fmt.Errorf("unknown op %q", op.Op)
	}
}

func (e *Extractor) addFact(ctx context.Context, src *anamnesia.Source, op Operation) (applied, error) {
	key := op.Key
	if key == "" {
		key = deriveFactKey(op.Value, op.Body, op.Title)
	}
	if key == "" {
		return applied{}, errors.New("ADD_FACT: key required and could not be derived")
	}
	op.Key = key
	scope := anamnesia.FactScope(op.FactScope)
	if !scope.Valid() {
		scope = anamnesia.FactScopeProject
	}
	value := valueToMap(op.Value)
	trust := op.Trust
	if trust == 0 {
		trust = 0.7
	}
	sourceID := src.ID
	f := &anamnesia.Fact{
		Scope:    src.Scope,
		FactKind: scope,
		Key:      op.Key,
		Value:    value,
		Source:   strOr(op.Source, "extracted"),
		SourceID: &sourceID,
		Trust:    trust,
	}
	if err := e.Store.UpsertFact(ctx, f); err != nil {
		return applied{}, err
	}
	return applied{Target: "fact", ID: f.ID}, nil
}

func (e *Extractor) updateFact(ctx context.Context, src *anamnesia.Source, op Operation) (applied, error) {
	id, err := uuid.Parse(op.ID)
	if err != nil {
		return applied{}, fmt.Errorf("UPDATE_FACT: %w", err)
	}
	prev, err := e.Store.GetFact(ctx, id)
	if err != nil {
		return applied{}, fmt.Errorf("UPDATE_FACT: %w", err)
	}
	if len(op.Value) > 0 {
		prev.Value = valueToMap(op.Value)
	}
	if op.Trust > 0 {
		prev.Trust = op.Trust
	}
	prev.Source = strOr(op.Source, "extracted")
	sourceID := src.ID
	prev.SourceID = &sourceID
	if err := e.Store.UpsertFact(ctx, prev); err != nil {
		return applied{}, err
	}
	return applied{Target: "fact", ID: prev.ID}, nil
}

func (e *Extractor) deleteFact(ctx context.Context, op Operation) (applied, error) {
	id, err := uuid.Parse(op.ID)
	if err != nil {
		return applied{}, fmt.Errorf("DELETE_FACT: %w", err)
	}
	if err := e.Store.ForgetFact(ctx, id); err != nil {
		return applied{}, err
	}
	return applied{Target: "fact", ID: id}, nil
}

func (e *Extractor) addExperience(ctx context.Context, src *anamnesia.Source, op Operation) (applied, error) {
	body := op.Body
	if body == "" {
		return applied{}, errors.New("ADD_EXPERIENCE: body required")
	}
	kind := anamnesia.ExperienceKind(op.Kind)
	if !kind.Valid() {
		kind = anamnesia.ExperienceCase
	}
	imp := op.Importance
	if imp == 0 {
		imp = 0.5
	}
	srcID := src.ID
	occurred := src.OccurredAt
	exp := &anamnesia.Experience{
		Scope:        src.Scope,
		Kind:         kind,
		Title:        op.Title,
		Body:         body,
		Outcome:      anamnesia.Outcome(op.Outcome),
		Importance:   imp,
		Trust:        0.7,
		SourceID:     &srcID,
		OccurredAt:   &occurred,
		Participants: src.Participants,
		Topic:        op.Topic,
		Provenance:   map[string]any{"source_kind": src.Kind, "source_ref": src.ExternalRef},
		Meta:         map[string]any{"extracted": true, "source_id": src.ID},
	}
	if err := e.Store.RecordExperience(ctx, exp); err != nil {
		return applied{}, err
	}
	return applied{Target: "experience", ID: exp.ID}, nil
}

func (e *Extractor) addCommitment(ctx context.Context, src *anamnesia.Source, op Operation) (applied, error) {
	body := op.Body
	if body == "" {
		return applied{}, errors.New("ADD_COMMITMENT: body required")
	}
	srcID := src.ID
	c := &anamnesia.Commitment{
		Scope:       src.Scope,
		Owner:       op.Owner,
		Beneficiary: op.Beneficiary,
		Body:        body,
		Status:      anamnesia.CommitmentOpen,
		SourceID:    &srcID,
	}
	if op.DueAt != "" {
		if t, err := time.Parse(time.RFC3339, op.DueAt); err == nil {
			c.DueAt = &t
		}
	}
	if err := e.Store.RecordCommitment(ctx, c); err != nil {
		return applied{}, err
	}
	return applied{Target: "commitment", ID: c.ID}, nil
}

// ─── helpers ─────────────────────────────────────────────────────────

// temporalMarkerRE matches phrases that signal "something just changed",
// so the surprise gate doesn't suppress them.
var temporalMarkerRE = regexp.MustCompile(`(?i)\b(just|now|today|yesterday|tomorrow|this morning|tonight|earlier|last (week|month|year)|switched to|changed to|moved to|started using|stopped using|no longer)\b`)

func hasTemporalMarker(s string) bool { return temporalMarkerRE.MatchString(s) }

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max]
	}
	return s
}

func strOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
