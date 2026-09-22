package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A reasoning model spends its budget thinking about every checkpoint, and
// extraction does not need it: measured 2026-09-22, gpt-5-nano ran at
// roughly a minute per source against about four seconds for gpt-4o-mini,
// which is what made it unusable rather than merely slower. The parameter
// is how you buy that back, and sending it when nobody asked would change
// the behaviour of every model that has a default of its own.
func TestReasoningEffortIsSentOnlyWhenConfigured(t *testing.T) {
	cases := []struct {
		name, configured, want string
	}{
		{"unset sends no reasoning field at all", "", ""},
		{"minimal", "minimal", "minimal"},
		{"low", "low", "low"},
		{"high", "high", "high"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got oaiChatReq
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`))
			}))
			t.Cleanup(srv.Close)

			o := &openaiLLM{model: "test-model", baseURL: srv.URL, apiKey: "test", reasoningEffort: c.configured}
			var out map[string]any
			if err := o.Extract(context.Background(), DistillInput{System: "s", User: "u"}, &out); err != nil {
				t.Fatalf("extract: %v", err)
			}

			if c.want == "" {
				if got.Reasoning != nil {
					t.Errorf("sent reasoning %+v with nothing configured", got.Reasoning)
				}
				return
			}
			if got.Reasoning == nil {
				t.Fatalf("reasoning effort %q was configured but not sent", c.configured)
			}
			if got.Reasoning.Effort != c.want {
				t.Errorf("effort = %q, want %q", got.Reasoning.Effort, c.want)
			}
		})
	}
}

// Every call the extractor and the workers make goes through the same
// request builder, so the setting must not apply to one path only.
func TestReasoningEffortAppliesToEveryCall(t *testing.T) {
	for _, call := range []string{"Complete", "Distill", "Extract"} {
		t.Run(call, func(t *testing.T) {
			var got oaiChatReq
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`))
			}))
			t.Cleanup(srv.Close)

			o := &openaiLLM{model: "test-model", baseURL: srv.URL, apiKey: "test", reasoningEffort: "low"}
			var out map[string]any
			var err error
			switch call {
			case "Complete":
				_, err = o.Complete(context.Background(), "p")
			case "Distill":
				err = o.Distill(context.Background(), DistillInput{System: "s", User: "u"}, &out)
			case "Extract":
				err = o.Extract(context.Background(), DistillInput{System: "s", User: "u"}, &out)
			}
			if err != nil {
				t.Fatalf("%s: %v", call, err)
			}
			if got.Reasoning == nil || got.Reasoning.Effort != "low" {
				t.Errorf("%s did not carry the configured reasoning effort", call)
			}
		})
	}
}
