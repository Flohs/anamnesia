package store

import (
	"strings"
	"testing"

	"github.com/flohs/anamnesia/pkg/anamnesia"
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
