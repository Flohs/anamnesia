package httpapi

import (
	"encoding/json"
	"testing"
)

// TestHookEventDecodesIncludeHistory pins the wire name. The engine side
// is covered in internal/retrieval; what this guards is the field being
// reachable at all from a caller, and defaulting to off so the hooks that
// inject memory into every prompt never see superseded values.
func TestHookEventDecodesIncludeHistory(t *testing.T) {
	var ev HookEvent
	if err := json.Unmarshal([]byte(`{"prompt":"x","include_history":true}`), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !ev.IncludeHistory {
		t.Error("include_history did not decode: the flag is unreachable over HTTP")
	}
}

func TestIncludeHistoryDefaultsToOff(t *testing.T) {
	var ev HookEvent
	if err := json.Unmarshal([]byte(`{"prompt":"x"}`), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.IncludeHistory {
		t.Error("include_history defaulted to true: every injected prompt would carry stale values")
	}
}
