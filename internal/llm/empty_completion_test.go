package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// oaiStub serves one canned chat-completions response.
func oaiStub(t *testing.T, body string) *openaiLLM {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &openaiLLM{model: "test-model", baseURL: srv.URL, apiKey: "test"}
}

// TestAnEmptyCompletionSaysSo is the 32 extraction failures in the last
// benchmark corpus. Every one reported "unexpected end of JSON input",
// which is what encoding/json returns for an EMPTY input — the model
// returned no content at all. The error named the symptom (some JSON
// somewhere was cut short) instead of the cause, which sent the
// investigation looking for a truncated response that never existed.
func TestAnEmptyCompletionSaysSo(t *testing.T) {
	o := oaiStub(t, `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)

	var out map[string]any
	err := o.Extract(context.Background(), DistillInput{System: "s", User: "u"}, &out)
	if err == nil {
		t.Fatal("an empty completion was accepted")
	}
	if strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Errorf("error still reports the JSON symptom rather than the cause: %v", err)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %v, want it to say the completion was empty", err)
	}
}

// TestAnEmptyCompletionReportsWhyTheModelStopped: finish_reason is the
// one piece of evidence that separates "the model was cut off at
// max_tokens" from "the provider returned nothing". It was parsed
// nowhere, so both looked identical.
func TestAnEmptyCompletionReportsWhyTheModelStopped(t *testing.T) {
	o := oaiStub(t, `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"length"}]}`)

	var out map[string]any
	err := o.Extract(context.Background(), DistillInput{System: "s", User: "u"}, &out)
	if err == nil {
		t.Fatal("an empty completion was accepted")
	}
	if !strings.Contains(err.Error(), "length") {
		t.Errorf("error = %v, want finish_reason in it: without it, a token cap and a dead provider are indistinguishable", err)
	}
}

// TestAWhitespaceOnlyCompletionCountsAsEmpty: the fence-stripping in
// Distill trims to "" for a response that is only whitespace or an empty
// code fence, which reaches json.Unmarshal identically.
func TestAWhitespaceOnlyCompletionCountsAsEmpty(t *testing.T) {
	fenced, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "```json\n```"},
			"finish_reason": "stop",
		}},
	})
	o := oaiStub(t, string(fenced))

	var out map[string]any
	err := o.Extract(context.Background(), DistillInput{System: "s", User: "u"}, &out)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %v, want an empty-completion error", err)
	}
}

// TestAGoodCompletionStillParses guards the over-correction.
func TestAGoodCompletionStillParses(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"operations": []any{}})
	body, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": string(payload)},
			"finish_reason": "stop",
		}},
	})
	o := oaiStub(t, string(body))

	var out struct {
		Operations []any `json:"operations"`
	}
	if err := o.Extract(context.Background(), DistillInput{System: "s", User: "u"}, &out); err != nil {
		t.Fatalf("a valid completion failed: %v", err)
	}
}

// TestStructuredOutputIsStrict pins the flag on.
//
// It was deliberately off until 2026-09-22, because OpenAI rejects a
// strict schema unless every object carries additionalProperties:false
// and every property appears in required, and the operations schema had
// a dozen fields that only applied to some ops. That rewrite has since
// happened (internal/extract), driven by newer OpenAI models enforcing
// the same rules whether or not strict is asked for.
//
// With the schemas compliant, strict is no longer a trade: it makes a
// non-conforming response impossible rather than merely unlikely. The
// cost it still carries is that a caller passing a looser schema of its
// own now gets a 400 instead of a best effort, which is why
// internal/extract has a test walking every schema it sends.
func TestStructuredOutputIsStrict(t *testing.T) {
	var got oaiChatReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(srv.Close)
	o := &openaiLLM{model: "test-model", baseURL: srv.URL, apiKey: "test"}

	var out map[string]any
	if err := o.Extract(context.Background(), DistillInput{
		System: "s", User: "u",
		Schema:     json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		SchemaName: "test_schema",
	}, &out); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.ResponseFormat == nil || got.ResponseFormat.JSONSchema == nil {
		t.Fatal("no json_schema was sent")
	}
	if !got.ResponseFormat.JSONSchema.Strict {
		t.Error("strict was not set: a non-conforming response stays merely unlikely instead of impossible, and the schemas have supported it since 2026-09-22")
	}
}
