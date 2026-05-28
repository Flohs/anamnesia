package anamnesia

import "time"

// BriefingRequest is the input shape to the /v1/briefing endpoint and
// the anamnesia_briefing MCP tool. Since is required; Until=zero means
// "now". Topic narrows the window to experiences whose title or topic
// column prefix-matches; MaxAdjacent caps the "you might also want to
// mention…" list (default 3).
type BriefingRequest struct {
	Scope       Scope     `json:"scope"`
	Since       time.Time `json:"since"`
	Until       time.Time `json:"until,omitempty"`
	Topic       string    `json:"topic,omitempty"`
	MaxAdjacent int       `json:"max_adjacent,omitempty"`
}

// BriefingResponse is what a dock-on agent receives when it asks
// "what's been going on". Summary is one paragraph of prose;
// Highlights are the load-bearing items inside the window; Adjacent
// are items near the window (in time or topic) that the agent might
// want to surface alongside the summary.
type BriefingResponse struct {
	Window     Window         `json:"window"`
	Summary    string         `json:"summary"`
	Highlights []BriefingItem `json:"highlights"`
	Adjacent   []BriefingItem `json:"adjacent"`
}

type Window struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

type BriefingItem struct {
	Title string `json:"title"`
	Why   string `json:"why"`
	Kind  string `json:"kind,omitempty"`
}
