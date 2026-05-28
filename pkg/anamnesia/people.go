package anamnesia

import "time"

// PersonView is the boot-shaped read view of an entity-of-kind-person:
// the entity itself, a recent-mention count to surface relevance, and
// a small slice of outgoing edges so an agent can answer "who do I know"
// without a follow-up graph query.
type PersonView struct {
	Entity          *Entity      `json:"entity"`
	RecentMentions  int          `json:"recent_mentions"`
	LastMentionedAt *time.Time   `json:"last_mentioned_at,omitempty"`
	Edges           []PersonEdge `json:"edges,omitempty"`
}

type PersonEdge struct {
	Kind   string `json:"kind"`
	ToName string `json:"to_name"`
	ToID   string `json:"to_id"`
}
