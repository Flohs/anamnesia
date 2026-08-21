package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// recordingOAI captures the max_tokens of every request and replies with
// a scripted body per call.
func recordingOAI(t *testing.T, bodies ...string) (*openaiLLM, *[]int) {
	t.Helper()
	var mu sync.Mutex
	var caps []int
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req oaiChatReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		caps = append(caps, req.MaxTokens)
		i := n
		n++
		mu.Unlock()
		if i >= len(bodies) {
			i = len(bodies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bodies[i]))
	}))
	t.Cleanup(srv.Close)
	return &openaiLLM{model: "m", baseURL: srv.URL, apiKey: "k"}, &caps
}

// TestATruncatedCompletionIsRetriedWithMoreRoom is the difference between
// the two failure modes behind "unexpected end of JSON input". Garbage is
// transient and a plain retry fixes it. Truncation is not: the same
// source and the same prompt overflow the same budget every time, so
// retrying at the same cap just spends the call again. finish_reason
// "length" is the model saying which one happened.
//
// Measured over 24 real sources, extraction emits a median of 198 tokens
// under the default prompt and 525 under the liberal one, so escalating
// only on "length" leaves the common case untouched and pays more only
// where it is the difference between a memory and a hole.
func TestATruncatedCompletionIsRetriedWithMoreRoom(t *testing.T) {
	o, caps := recordingOAI(t,
		completion(`{"operations": [{"op":"ADD_`, "length"),
		completion(`{"operations":[]}`, "stop"),
	)
	var out map[string]any
	if err := o.Extract(context.Background(), DistillInput{System: "s", User: "u", MaxTok: 1024}, &out); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(*caps) < 2 {
		t.Fatalf("requests = %d, want a retry", len(*caps))
	}
	if (*caps)[1] <= (*caps)[0] {
		t.Errorf("max_tokens went %d -> %d: a retry at the same budget truncates again",
			(*caps)[0], (*caps)[1])
	}
}

// TestANonTruncatedRetryKeepsTheSameBudget: garbage that is not a
// truncation should not silently inflate the token bill.
func TestANonTruncatedRetryKeepsTheSameBudget(t *testing.T) {
	o, caps := recordingOAI(t,
		completion(`not json at all`, "stop"),
		completion(`{"operations":[]}`, "stop"),
	)
	var out map[string]any
	if err := o.Extract(context.Background(), DistillInput{System: "s", User: "u", MaxTok: 1024}, &out); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(*caps) >= 2 && (*caps)[1] != (*caps)[0] {
		t.Errorf("max_tokens went %d -> %d for a non-truncation retry", (*caps)[0], (*caps)[1])
	}
}

// TestTheEscalatedBudgetIsBounded: an unbounded doubling would turn one
// pathological source into an arbitrarily large bill.
func TestTheEscalatedBudgetIsBounded(t *testing.T) {
	o, caps := recordingOAI(t, completion(`{"operations": [{"op":"ADD_`, "length"))
	var out map[string]any
	_ = o.Extract(context.Background(), DistillInput{System: "s", User: "u", MaxTok: 1024}, &out)
	for _, c := range *caps {
		if c > 16384 {
			t.Errorf("max_tokens reached %d: the escalation is unbounded", c)
		}
	}
}
