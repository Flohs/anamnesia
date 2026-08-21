package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// scriptedOAI serves a sequence of canned bodies, one per call, repeating
// the last forever. Counts calls so a retry is observable.
func scriptedOAI(t *testing.T, bodies ...string) (*openaiLLM, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(atomic.AddInt32(&calls, 1)) - 1
		if n >= len(bodies) {
			n = len(bodies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bodies[n]))
	}))
	t.Cleanup(srv.Close)
	return &openaiLLM{model: "m", baseURL: srv.URL, apiKey: "k"}, &calls
}

func completion(content, finish string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": finish,
	}}})
	return string(b)
}

// TestATransientUnparseableResponseIsRetried is the instability itself.
// Retrying the 32 failed sources of a benchmark corpus, unchanged, made
// 28 of them succeed: same source, same model, same prompt. Treating one
// bad completion as a permanent failure is what turned a model hiccup
// into a hole in memory, and is why one run lost 14% of sources and
// another 1.2%.
func TestATransientUnparseableResponseIsRetried(t *testing.T) {
	o, calls := scriptedOAI(t,
		completion(`{"operations": [{"op":"ADD_`, "length"),
		completion(`{"operations":[]}`, "stop"),
	)
	var out struct {
		Operations []any `json:"operations"`
	}
	if err := o.Extract(context.Background(), DistillInput{System: "s", User: "u"}, &out); err != nil {
		t.Fatalf("a retryable response was not retried: %v", err)
	}
	if *calls < 2 {
		t.Errorf("calls = %d, want a second attempt", *calls)
	}
}

// TestAPersistentlyUnparseableResponseStillFails: retrying forever would
// hang a queue behind one poisoned source.
func TestAPersistentlyUnparseableResponseStillFails(t *testing.T) {
	o, calls := scriptedOAI(t, completion(`{"operations": [{"op":"ADD_`, "length"))
	var out map[string]any
	if err := o.Extract(context.Background(), DistillInput{System: "s", User: "u"}, &out); err == nil {
		t.Fatal("a permanently broken response was accepted")
	}
	if *calls > 4 {
		t.Errorf("calls = %d: retries are not bounded", *calls)
	}
}

// TestAGoodResponseIsNotRetried guards against paying twice for every
// extraction.
func TestAGoodResponseIsNotRetried(t *testing.T) {
	o, calls := scriptedOAI(t, completion(`{"operations":[]}`, "stop"))
	var out map[string]any
	if err := o.Extract(context.Background(), DistillInput{System: "s", User: "u"}, &out); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want exactly 1", *calls)
	}
}

// TestAParseFailureReportsWhyTheModelStopped: "unexpected end of JSON
// input" names the symptom. Whether the model was cut off at max_tokens
// or the provider returned junk is the whole question, and finish_reason
// is the only thing that answers it.
func TestAParseFailureReportsWhyTheModelStopped(t *testing.T) {
	o, _ := scriptedOAI(t, completion(`{"operations": [{"op":"ADD_`, "length"))
	var out map[string]any
	err := o.Extract(context.Background(), DistillInput{System: "s", User: "u"}, &out)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "length") {
		t.Errorf("error = %v, want finish_reason in it", err)
	}
	if !strings.Contains(err.Error(), "26") && !strings.Contains(err.Error(), "bytes") {
		t.Errorf("error = %v, want the response size, which is what shows a cut-off", err)
	}
}
