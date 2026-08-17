package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/flohs/anamnesia/internal/llm"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

const briefingSystemPrompt = `You are a personal-assistant brief writer. Given a list of experiences in a time window plus "adjacent" experiences from before or related topics, produce JSON:
{"summary":"<one paragraph, plain prose>","highlights":[{"title":"...","why":"..."}],"adjacent":[{"title":"...","why":"you might also want to mention this because ...","kind":"experience"}]}
Limit highlights to <=5 and adjacent to the MaxAdjacent provided. No prose outside JSON.`

// Briefer composes a temporal-window summary plus adjacent items by
// asking the LLM. Nil LLM falls back to a deterministic stub summary
// so the briefing endpoint stays reachable in offline / stub mode.
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
	until := req.Until
	if until.IsZero() {
		until = time.Now().UTC()
	}
	user := fmt.Sprintf("Window: %s — %s\nMaxAdjacent: %d\nTopic: %q\n\n",
		req.Since.Format("2006-01-02"), until.Format("2006-01-02"), maxAdj, req.Topic)
	user += "In-window experiences:\n" + renderExperiencesForPrompt(inWindow) + "\n"
	user += "Adjacent experiences:\n" + renderExperiencesForPrompt(adjacent) + "\n"
	user += "\nRespond with the JSON object only."

	out := anamnesia.BriefingResponse{
		Window: anamnesia.Window{Since: req.Since, Until: until},
	}
	if b.LLM == nil {
		out.Summary = fmt.Sprintf("(stub) %d experiences in window, %d adjacent",
			len(inWindow), len(adjacent))
		return out, nil
	}
	var parsed struct {
		Summary    string                   `json:"summary"`
		Highlights []anamnesia.BriefingItem `json:"highlights"`
		Adjacent   []anamnesia.BriefingItem `json:"adjacent"`
	}
	if err := b.LLM.Distill(ctx, llm.DistillInput{
		System: briefingSystemPrompt,
		User:   user,
	}, &parsed); err != nil {
		return out, fmt.Errorf("briefing llm: %w", err)
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

// jsonRoundTrip is a small helper used by tests to unmarshal a literal
// JSON byte slice into the same destination shape Distill would.
func jsonRoundTrip(raw []byte, out any) error {
	return json.Unmarshal(raw, out)
}
