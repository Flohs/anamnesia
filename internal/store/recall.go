// recall.go keeps the tally of how often retrieval came back with
// something.
//
// One retrieval is one of three outcomes, and the distinction that
// matters is between a miss and an outcome there was no way to judge.
// Fused RRF scores are rank-based, so "we returned ten rows" says
// nothing about whether any of them matched; only the reranker's score
// or a cosine distance is absolute. Where neither exists the honest
// answer is ungraded, not a pass.
package store

import (
	"context"

	"github.com/google/uuid"
)

// RecallOutcome is how one retrieval turned out.
type RecallOutcome string

const (
	// RecallHit is a retrieval that returned at least one hit clearing
	// the absolute bar.
	RecallHit RecallOutcome = "recalled"
	// RecallMiss is a retrieval that was judged and cleared nothing.
	RecallMiss RecallOutcome = "missed"
	// RecallUngraded is a retrieval with no absolute number to judge by:
	// no embedder configured, or no returned hit came from the vector
	// channel.
	RecallUngraded RecallOutcome = "ungraded"
)

// RecallCounts is a window of the tally. Misses are Prompts - Recalled -
// Ungraded, derived rather than stored.
type RecallCounts struct {
	Days     int `json:"days"`
	Prompts  int `json:"prompts"`
	Recalled int `json:"recalled"`
	Ungraded int `json:"ungraded"`
}

// RecordRetrieval adds one retrieval to today's row.
//
// The day is taken from the database in UTC rather than from the Go
// clock, so a window query and the rows it reads can never disagree
// about which day a retrieval fell in.
func (s *Store) RecordRetrieval(ctx context.Context, userID uuid.UUID, outcome RecallOutcome) error {
	var recalled, ungraded int
	switch outcome {
	case RecallHit:
		recalled = 1
	case RecallUngraded:
		ungraded = 1
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO recall_daily (user_id, day, prompts, recalled, ungraded)
		VALUES ($1, (now() AT TIME ZONE 'utc')::date, 1, $2, $3)
		ON CONFLICT (user_id, day) DO UPDATE SET
			prompts  = recall_daily.prompts  + 1,
			recalled = recall_daily.recalled + EXCLUDED.recalled,
			ungraded = recall_daily.ungraded + EXCLUDED.ungraded`,
		userID, recalled, ungraded)
	return err
}

// RecallWindow sums the last `days` days, today included.
func (s *Store) RecallWindow(ctx context.Context, userID uuid.UUID, days int) (RecallCounts, error) {
	out := RecallCounts{Days: days}
	err := s.Pool.QueryRow(ctx, `
		SELECT coalesce(sum(prompts), 0), coalesce(sum(recalled), 0), coalesce(sum(ungraded), 0)
		FROM recall_daily
		WHERE user_id = $1 AND day > (now() AT TIME ZONE 'utc')::date - $2::int`,
		userID, days).Scan(&out.Prompts, &out.Recalled, &out.Ungraded)
	if err != nil {
		return RecallCounts{Days: days}, err
	}
	return out, nil
}
