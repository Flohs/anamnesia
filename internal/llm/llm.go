// Package llm wraps a chat-completions style LLM. Four implementations:
// anthropic (Claude), openai (any OpenAI-compatible chat endpoint —
// OpenAI, vLLM, Ollama, Azure), openrouter (OpenAI-compatible alias
// for openrouter.ai with attribution headers preset), and stub
// (deterministic). The consolidation and extraction workers both use
// this surface.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is the minimal surface used by consolidation and extraction.
type Client interface {
	Complete(ctx context.Context, prompt string) (string, error)
	// Distill runs the given system + user prompt and unmarshals the
	// model's JSON response into out. Used by the consolidation worker.
	Distill(ctx context.Context, in DistillInput, out any) error
	// Extract runs a structured prompt against the model and decodes
	// JSON into out. Used by the ingest extractor; semantically the
	// same as Distill but kept as a separate method so each path can
	// tune prompt caching and max-tokens independently.
	Extract(ctx context.Context, in DistillInput, out any) error
	Model() string
}

// DistillInput is the prompt shape for consolidation calls.
type DistillInput struct {
	System string
	User   string
	MaxTok int
	// Schema, when non-empty, is sent as the OpenAI response_format
	// json_schema for the call. The Anthropic path ignores it (the
	// prompt alone is expected to enforce shape there). Use this to
	// pin the model's output to a known JSON shape and eliminate
	// shape-drift bugs.
	Schema json.RawMessage
	// SchemaName labels the schema in OpenAI's response_format. Defaults
	// to "structured_output" when empty.
	SchemaName string
}

// Config bundles everything every provider might need. Unused fields
// are ignored by providers that don't need them (e.g. anthropic ignores
// BaseURL — the API URL is hard-coded).
type Config struct {
	Provider string
	Model    string
	APIKey   string
	BaseURL  string // OpenAI-compatible endpoints only
	// Timeout is the per-request HTTP timeout. Zero means the default
	// below, which is generous enough for hosted APIs; a local Ollama
	// with a cold model can need far more on its first call.
	Timeout time.Duration
}

// defaultLLMTimeout applies when Config.Timeout is unset.
const defaultLLMTimeout = 120 * time.Second

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultLLMTimeout
}

// New returns a Client for the configured provider.
func New(cfg Config) (Client, error) {
	switch cfg.Provider {
	case "anthropic":
		if cfg.APIKey == "" {
			return nil, errors.New("anthropic: ANTHROPIC_API_KEY required")
		}
		return &anthropic{apiKey: cfg.APIKey, model: cfg.Model, timeout: cfg.timeout()}, nil
	case "openai":
		if cfg.APIKey == "" {
			return nil, errors.New("openai: OPENAI_API_KEY required")
		}
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		return &openaiLLM{apiKey: cfg.APIKey, baseURL: baseURL, model: cfg.Model, timeout: cfg.timeout()}, nil
	case "openrouter":
		if cfg.APIKey == "" {
			return nil, errors.New("openrouter: OPENROUTER_API_KEY required")
		}
		return &openaiLLM{
			apiKey:       cfg.APIKey,
			baseURL:      OpenRouterBaseURL,
			model:        cfg.Model,
			extraHeaders: OpenRouterHeaders(),
			timeout:      cfg.timeout(),
		}, nil
	case "stub", "":
		return &stubLLM{model: "stub"}, nil
	default:
		return nil, errors.New("unknown llm provider: " + cfg.Provider)
	}
}

type stubLLM struct{ model string }

func (s *stubLLM) Model() string { return s.model }
func (s *stubLLM) Complete(_ context.Context, prompt string) (string, error) {
	first := prompt
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	if len(first) > 200 {
		first = first[:200]
	}
	return "stub-distillation: " + first, nil
}

// Distill returns a deterministic placeholder so the worker loop can
// exercise the persistence path without an LLM in the loop.
func (s *stubLLM) Distill(_ context.Context, in DistillInput, out any) error {
	stub := map[string]any{
		"title":      "stub distillation",
		"body":       "Placeholder body — configure ANAMNESIA_LLM_PROVIDER=anthropic to get real consolidation.",
		"outcome":    "",
		"importance": 0.5,
		"kind":       "case",
	}
	raw, _ := json.Marshal(stub)
	return json.Unmarshal(raw, out)
}

// Extract returns no operations by default. With the stub LLM, ingest
// is a write-through to `sources` with nothing extracted — exactly
// what we want for offline / dev testing.
func (s *stubLLM) Extract(_ context.Context, in DistillInput, out any) error {
	stub := map[string]any{"operations": []any{}}
	raw, _ := json.Marshal(stub)
	return json.Unmarshal(raw, out)
}

type anthropic struct {
	apiKey  string
	model   string
	timeout time.Duration
	hc      *http.Client
}

func (a *anthropic) Model() string { return a.model }

func (a *anthropic) client() *http.Client {
	if a.hc == nil {
		a.hc = &http.Client{Timeout: a.timeout}
	}
	return a.hc
}

type anthReq struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []anthMsg `json:"messages"`
}

type anthMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthResp struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Distill calls the Claude messages API and unmarshals the JSON response
// into out. The system prompt is sent as a top-level "system" string so
// it benefits from prompt caching across cluster calls.
func (a *anthropic) Distill(ctx context.Context, in DistillInput, out any) error {
	body, err := json.Marshal(struct {
		Model     string    `json:"model"`
		MaxTokens int       `json:"max_tokens"`
		System    string    `json:"system,omitempty"`
		Messages  []anthMsg `json:"messages"`
	}{
		Model:     a.model,
		MaxTokens: maxOr(in.MaxTok, 1024),
		System:    in.System,
		Messages:  []anthMsg{{Role: "user", Content: in.User}},
	})
	if err != nil {
		return err
	}
	rb, status, err := doRetry(ctx, a.client(), func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST",
			"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", a.apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}, 5)
	if err != nil {
		return fmt.Errorf("anthropic: %w", err)
	}
	if status != 200 {
		return fmt.Errorf("anthropic: status %d: %s", status, string(rb))
	}
	var parsed anthResp
	if err := json.Unmarshal(rb, &parsed); err != nil {
		return fmt.Errorf("anthropic: parse %w", err)
	}
	if parsed.Error != nil {
		return fmt.Errorf("anthropic: %s", parsed.Error.Message)
	}
	var text strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	// Claude often wraps JSON in ```json fences. Strip them before
	// unmarshalling so the worker doesn't need to know about the
	// formatting.
	s := strings.TrimSpace(text.String())
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	return json.Unmarshal([]byte(s), out)
}

// Extract reuses Distill's request shape. The caller is expected to
// pass a system prompt that asks for JSON with an "operations" array.
func (a *anthropic) Extract(ctx context.Context, in DistillInput, out any) error {
	return a.Distill(ctx, in, out)
}

// ─── OpenAI-compatible chat completions ───────────────────────────────
//
// Works against any endpoint that speaks OpenAI's /chat/completions
// surface: api.openai.com, OpenRouter, vLLM, Ollama (>=0.5), Azure
// OpenAI (with the right deployment name). The base URL is provided by
// config so the same code paths cover all four.

// OpenRouterBaseURL is the OpenAI-compatible base URL for openrouter.ai.
// Exported so the embed and rerank backends can share it.
const OpenRouterBaseURL = "https://openrouter.ai/api/v1"

// OpenRouterHeaders returns the optional attribution headers OpenRouter
// uses to bucket app traffic on its public leaderboard. Safe to send on
// every request; harmless on other OpenAI-compatible endpoints (they
// ignore unknown headers).
func OpenRouterHeaders() map[string]string {
	return map[string]string{
		"HTTP-Referer": "https://github.com/flohs/anamnesia",
		"X-Title":      "Anamnesia",
	}
}

type openaiLLM struct {
	apiKey       string
	baseURL      string
	model        string
	extraHeaders map[string]string
	timeout      time.Duration
	hc           *http.Client
}

func (o *openaiLLM) Model() string { return o.model }

func (o *openaiLLM) client() *http.Client {
	if o.hc == nil {
		o.hc = &http.Client{Timeout: o.timeout}
	}
	return o.hc
}

type oaiMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaiChatReq struct {
	Model          string      `json:"model"`
	Messages       []oaiMsg    `json:"messages"`
	MaxTokens      int         `json:"max_tokens,omitempty"`
	ResponseFormat *oaiRespFmt `json:"response_format,omitempty"`
}

type oaiRespFmt struct {
	Type       string            `json:"type"`
	JSONSchema *oaiJSONSchemaFmt `json:"json_schema,omitempty"`
}

type oaiJSONSchemaFmt struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict,omitempty"`
	Schema json.RawMessage `json:"schema"`
}

type oaiChatResp struct {
	Choices []struct {
		Message oaiMsg `json:"message"`
		// FinishReason separates "the model was cut off at max_tokens"
		// ("length") from "the provider returned nothing" ("stop" with no
		// content). Discarding it made both surface as the same opaque
		// JSON parse error.
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type,omitempty"`
	} `json:"error,omitempty"`
}

func (o *openaiLLM) chat(ctx context.Context, messages []oaiMsg, maxTok int, schema json.RawMessage, schemaName string, wantJSON bool) (content, finish string, err error) {
	body := oaiChatReq{
		Model:     o.model,
		Messages:  messages,
		MaxTokens: maxOr(maxTok, 1024),
	}
	switch {
	case len(schema) > 0:
		name := schemaName
		if name == "" {
			name = "structured_output"
		}
		body.ResponseFormat = &oaiRespFmt{
			Type: "json_schema",
			JSONSchema: &oaiJSONSchemaFmt{
				Name: name,
				// Strict is deliberately NOT set. It would make a
				// non-conforming response impossible instead of merely
				// unlikely, but OpenAI rejects a strict schema unless
				// every object carries additionalProperties:false and
				// every property is listed in required. The operations
				// schema has a dozen fields that only apply to some ops,
				// so enabling it means rewriting the schema to demand
				// nulls for all of them, which changes what the model
				// emits. Verified against the live API on 2026-08-21:
				// setting it returns 400 "Invalid schema for
				// response_format". See TestStructuredOutputIsNotStrict.
				Schema: schema,
			},
		}
	case wantJSON:
		body.ResponseFormat = &oaiRespFmt{Type: "json_object"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	rb, status, err := doRetry(ctx, o.client(), func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST",
			strings.TrimRight(o.baseURL, "/")+"/chat/completions", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range o.extraHeaders {
			req.Header.Set(k, v)
		}
		return req, nil
	}, 5)
	if err != nil {
		return "", "", fmt.Errorf("openai chat: %w", err)
	}
	if status != 200 {
		return "", "", fmt.Errorf("openai chat: status %d: %s", status, string(rb))
	}
	var parsed oaiChatResp
	if err := json.Unmarshal(rb, &parsed); err != nil {
		return "", "", fmt.Errorf("openai chat: parse %w", err)
	}
	if parsed.Error != nil {
		return "", "", fmt.Errorf("openai chat: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", "", errors.New("openai chat: no choices returned")
	}
	c := parsed.Choices[0]
	if strings.TrimSpace(c.Message.Content) == "" {
		return "", orUnknown(c.FinishReason), fmt.Errorf(
			"openai chat: model returned an empty completion (finish_reason %q)", orUnknown(c.FinishReason))
	}
	return c.Message.Content, orUnknown(c.FinishReason), nil
}

// orUnknown labels a finish_reason a provider omitted, so the error never
// reads as an empty quoted string.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (o *openaiLLM) Complete(ctx context.Context, prompt string) (string, error) {
	content, _, err := o.chat(ctx, []oaiMsg{{Role: "user", Content: prompt}}, 1024, nil, "", false)
	return content, err
}

// Distill runs the system + user prompt with JSON-mode forced on. Most
// OpenAI-compatible endpoints honour response_format={type:json_object};
// for those that don't, the JSON still parses because the system prompt
// also instructs the model to emit JSON.
// parseAttempts is how many times an unusable completion is re-requested
// before the source is given up on. A model that breaks its own output
// format does so transiently: re-running the 32 failed sources of a
// benchmark corpus unchanged made 28 of them succeed. One bad completion
// treated as permanent is a hole in memory, and is why one run lost 14%
// of its sources and another 1.2%. Bounded, because retrying forever
// would park the whole queue behind one poisoned source.
const parseAttempts = 3

// maxEscalatedTok bounds the budget growth below. One pathological source
// must not be able to run the bill up indefinitely.
const maxEscalatedTok = 16384

func (o *openaiLLM) Distill(ctx context.Context, in DistillInput, out any) error {
	msgs := []oaiMsg{}
	if in.System != "" {
		msgs = append(msgs, oaiMsg{Role: "system", Content: in.System})
	}
	msgs = append(msgs, oaiMsg{Role: "user", Content: in.User})

	// Truncation and garbage both surface as an unparseable body, and
	// they need opposite handling. Garbage is transient, so re-asking is
	// enough. Truncation is not: the same source and prompt overflow the
	// same budget every time, so a retry at the same cap burns another
	// call for the same result. finish_reason "length" is the model
	// saying which happened, so only that case buys more room.
	budget := in.MaxTok
	var lastErr error
	for attempt := 0; attempt < parseAttempts; attempt++ {
		raw, finish, err := o.chat(ctx, msgs, budget, in.Schema, in.SchemaName, true)
		if finish == "length" && budget < maxEscalatedTok {
			if budget <= 0 {
				budget = 1024
			}
			budget *= 2
			if budget > maxEscalatedTok {
				budget = maxEscalatedTok
			}
		}
		if err != nil {
			// A transport or provider error has already been retried by
			// doRetry; an empty completion is worth another attempt.
			lastErr = err
			if !strings.Contains(err.Error(), "empty completion") {
				return err
			}
			continue
		}
		// Belt-and-braces: strip ``` fences if a non-conforming endpoint
		// returned a fenced block despite JSON mode.
		s := strings.TrimSpace(raw)
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
		if s == "" {
			lastErr = fmt.Errorf("openai chat: model returned an empty completion, only a code fence (finish_reason %q)", finish)
			continue
		}
		if err := json.Unmarshal([]byte(s), out); err != nil {
			// Name the cause, not the symptom: "unexpected end of JSON
			// input" says nothing about whether the model was cut off at
			// max_tokens or returned junk. finish_reason and the size do.
			lastErr = fmt.Errorf("openai chat: could not parse the completion (finish_reason %q, %d bytes): %w",
				finish, len(s), err)
			continue
		}
		return nil
	}
	return lastErr
}

func (o *openaiLLM) Extract(ctx context.Context, in DistillInput, out any) error {
	return o.Distill(ctx, in, out)
}

func maxOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// doRetry executes the request, retrying on 429 and 5xx with exponential
// backoff up to maxAttempts. Honours the Retry-After header when present.
// Returns the final response body and status; on permanent failure returns
// the last error.
func doRetry(ctx context.Context, hc *http.Client, build func() (*http.Request, error), maxAttempts int) ([]byte, int, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		req, err := build()
		if err != nil {
			return nil, 0, err
		}
		res, err := hc.Do(req)
		if err != nil {
			lastErr = err
		} else {
			rb, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 429 && res.StatusCode < 500 {
				return rb, res.StatusCode, nil
			}
			lastErr = fmt.Errorf("%s: %s", res.Status, string(rb))
			if attempt+1 < maxAttempts {
				wait := retryAfter(res.Header.Get("Retry-After"), attempt)
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return nil, 0, ctx.Err()
				}
				continue
			}
		}
		if attempt+1 < maxAttempts {
			wait := time.Duration(1<<uint(attempt)) * time.Second
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
		}
	}
	return nil, 0, lastErr
}

func retryAfter(header string, attempt int) time.Duration {
	if header != "" {
		if d, err := time.ParseDuration(header + "s"); err == nil && d > 0 {
			return d
		}
	}
	return time.Duration(1<<uint(attempt)) * time.Second
}

func (a *anthropic) Complete(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(anthReq{
		Model:     a.model,
		MaxTokens: 1024,
		Messages:  []anthMsg{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", err
	}
	rb, status, err := doRetry(ctx, a.client(), func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST",
			"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", a.apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}, 5)
	if err != nil {
		return "", fmt.Errorf("anthropic: %w", err)
	}
	if status != 200 {
		return "", fmt.Errorf("anthropic: status %d: %s", status, string(rb))
	}
	var parsed anthResp
	if err := json.Unmarshal(rb, &parsed); err != nil {
		return "", fmt.Errorf("anthropic: parse %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("anthropic: %s", parsed.Error.Message)
	}
	var sb strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), nil
}
