package main

import (
	"math"
	"testing"
)

func closeTo(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestScoreCountsDistinctSourcesFound(t *testing.T) {
	// Two relevant sources; src-a is found twice (two rows from one
	// source) and must count once. src-b lands at rank 4.
	ranked := []string{"src-a", "src-x", "src-a", "src-b", "src-y"}
	s := score("q1", ranked, []string{"src-a", "src-b"}, []int{1, 5}, 120)

	closeTo(t, s.RecallAt[1], 0.5, "recall@1") // src-a only
	closeTo(t, s.RecallAt[5], 1.0, "recall@5") // both
	closeTo(t, s.MRR, 1.0, "MRR")              // first hit at rank 1
	if !s.Found {
		t.Error("Found = false, want true when anything relevant ranked")
	}
	if s.LatencyMS != 120 {
		t.Errorf("LatencyMS = %d, want 120", s.LatencyMS)
	}
}

func TestPrecisionCountsInjectedNoise(t *testing.T) {
	// Three of the top 5 are irrelevant. For an agent that is three
	// wasted slots of context, which is what precision measures.
	ranked := []string{"src-a", "src-x", "src-y", "src-b", "src-z"}
	s := score("q2", ranked, []string{"src-a", "src-b"}, []int{5}, 0)
	closeTo(t, s.PrecisionAt[5], 0.4, "precision@5")
}

func TestScoreWithNothingRelevantFound(t *testing.T) {
	s := score("q3", []string{"src-x", "src-y"}, []string{"src-a"}, []int{1, 5}, 0)
	closeTo(t, s.RecallAt[5], 0.0, "recall@5")
	closeTo(t, s.MRR, 0.0, "MRR")
	if s.Found {
		t.Error("Found = true, want false when no relevant source ranked")
	}
}

func TestScoreWithNoHitsAtAll(t *testing.T) {
	// A query that returned nothing must not divide by zero.
	s := score("q4", nil, []string{"src-a"}, []int{1, 5}, 0)
	closeTo(t, s.RecallAt[1], 0.0, "recall@1")
	closeTo(t, s.PrecisionAt[1], 0.0, "precision@1")
	closeTo(t, s.MRR, 0.0, "MRR")
}

func TestMRRUsesTheFirstRelevantRank(t *testing.T) {
	s := score("q5", []string{"src-x", "src-y", "src-a"}, []string{"src-a"}, []int{5}, 0)
	closeTo(t, s.MRR, 1.0/3.0, "MRR")
}

func TestAggregateReportsZeroHitQueriesAndPercentiles(t *testing.T) {
	scores := []queryScore{
		score("a", []string{"s1"}, []string{"s1"}, []int{1}, 100),
		score("b", []string{"s9"}, []string{"s1"}, []int{1}, 300),
		score("c", []string{"s1"}, []string{"s1"}, []int{1}, 200),
	}
	agg := aggregate(scores, []int{1})
	if agg.Queries != 3 {
		t.Errorf("Queries = %d, want 3", agg.Queries)
	}
	if agg.ZeroHit != 1 {
		t.Errorf("ZeroHit = %d, want 1: query b found nothing relevant", agg.ZeroHit)
	}
	closeTo(t, agg.RecallAt[1], 2.0/3.0, "mean recall@1")
	if agg.P50MS != 200 {
		t.Errorf("P50MS = %d, want 200", agg.P50MS)
	}
}
