package main

import (
	"strings"
	"testing"
)

func subagentInput(msg string) claudeHookInput {
	return claudeHookInput{
		SessionID:        "sess-1",
		CWD:              "/tmp/proj",
		AgentID:          "agent-7",
		AgentType:        "code-reviewer",
		LastAssistantMsg: msg,
		HookEventName:    "SubagentStop",
	}
}

// TestASubagentResultBecomesASource. A subagent runs in its own
// transcript, which no hook ever read, so everything it worked out was
// invisible to memory. One session here spawned 43 of them: they
// implemented features, reviewed branches and found defects, and none of
// that reasoning reached a single fact.
//
// Only the final message is taken. A subagent's transcript is as long as
// a session's, and a fan-out of forty would mean forty checkpoints; the
// conclusion is the part worth keeping.
func TestASubagentResultBecomesASource(t *testing.T) {
	p, ok := subagentPayload(subagentInput("The retry loop double-counts attempts."))
	if !ok {
		t.Fatal("a subagent that reported a finding produced no source")
	}
	if p.Content != "The retry loop double-counts attempts." {
		t.Errorf("content = %q, want the agent's final message", p.Content)
	}
	if p.Kind == "" {
		t.Error("the source has no kind, so nothing distinguishes it from a session checkpoint")
	}
}

// TestAnEmptySubagentResultIsNotIngested: an agent that was cancelled or
// died has nothing to say, and a source with no content still costs a
// surprise-gate call to discover that.
func TestAnEmptySubagentResultIsNotIngested(t *testing.T) {
	for _, msg := range []string{"", "   ", "\n\t "} {
		if _, ok := subagentPayload(subagentInput(msg)); ok {
			t.Errorf("an empty result (%q) was ingested", msg)
		}
	}
}

// TestSubagentSourcesAreDistinctPerAgent. external_ref is what stops one
// arrival being stored twice, so two agents in the same session must not
// share one, and the same agent must keep its own across a retry.
func TestSubagentSourcesAreDistinctPerAgent(t *testing.T) {
	a, _ := subagentPayload(subagentInput("first"))
	b := subagentInput("second")
	b.AgentID = "agent-8"
	second, _ := subagentPayload(b)

	if a.ExternalRef == "" {
		t.Fatal("no external_ref, so a repeated hook would store the same result twice")
	}
	if a.ExternalRef == second.ExternalRef {
		t.Errorf("two agents share the external_ref %q", a.ExternalRef)
	}
	again, _ := subagentPayload(subagentInput("first"))
	if again.ExternalRef != a.ExternalRef {
		t.Errorf("the same agent got two refs, %q then %q", a.ExternalRef, again.ExternalRef)
	}
}

// TestSubagentMetadataNamesTheAgent: which kind of agent said this is
// most of what makes the result interpretable later.
func TestSubagentMetadataNamesTheAgent(t *testing.T) {
	p, _ := subagentPayload(subagentInput("done"))
	if p.Metadata["agent_type"] != "code-reviewer" {
		t.Errorf("metadata = %v, want the agent type", p.Metadata)
	}
	if p.Metadata["session_id"] != "sess-1" {
		t.Errorf("metadata = %v, want the session the agent belonged to", p.Metadata)
	}
	if !strings.Contains(p.Title, "code-reviewer") {
		t.Errorf("title = %q, want the agent type in it", p.Title)
	}
}

// TestSubagentStopIsInstalled: the handler is unreachable unless the hook
// is registered, and install owns every entry running `anamnesia hook`.
func TestSubagentStopIsInstalled(t *testing.T) {
	var found bool
	for _, h := range anamnesiaHooks {
		if h.event == "SubagentStop" {
			found = true
			if h.verb != "subagent-stop" {
				t.Errorf("SubagentStop runs verb %q", h.verb)
			}
		}
	}
	if !found {
		t.Error("SubagentStop is not in the hook layout, so subagent results never arrive")
	}
	if _, ok := hookTimeouts["subagent-stop"]; !ok {
		t.Error("subagent-stop has no timeout budget, so it would get the zero value and fail instantly")
	}
}
