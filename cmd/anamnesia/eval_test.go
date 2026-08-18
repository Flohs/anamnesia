package main

import (
	"strings"
	"testing"
)

func TestReportNamesTheQueriesThatFoundNothing(t *testing.T) {
	// The aggregate can look healthy while individual queries return
	// nothing at all, and those are the sessions where memory silently
	// did not work. They have to be named, not averaged away.
	r := evalReport{
		Queries: 2,
		PerQuery: []queryScore{
			{ID: "q-001", Found: true, MRR: 1, RecallAt: map[int]float64{5: 1}, PrecisionAt: map[int]float64{5: 0.2}},
			{ID: "q-002", Found: false, RecallAt: map[int]float64{5: 0}, PrecisionAt: map[int]float64{5: 0}},
		},
		Aggregate: aggregateScore{
			Queries: 2, ZeroHit: 1, MRR: 0.5,
			RecallAt: map[int]float64{5: 0.5}, PrecisionAt: map[int]float64{5: 0.1},
		},
	}
	var sb strings.Builder
	renderReport(&sb, r)
	out := sb.String()
	if !strings.Contains(out, "q-002") {
		t.Errorf("report does not name the query that found nothing:\n%s", out)
	}
	if !strings.Contains(out, "recall@5") {
		t.Errorf("report omits recall@5:\n%s", out)
	}
}

func TestBaselineComparisonDetectsARecallRegression(t *testing.T) {
	base := evalReport{Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.80}, PrecisionAt: map[int]float64{5: 0.5}}}
	now := evalReport{Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.62}, PrecisionAt: map[int]float64{5: 0.5}}}
	regressed, lines := compareToBaseline(base, now)
	if !regressed {
		t.Error("a recall@5 drop from 0.80 to 0.62 was not reported as a regression")
	}
	if len(lines) == 0 {
		t.Error("comparison produced no explanation")
	}
}

func TestBaselineComparisonAcceptsAnImprovement(t *testing.T) {
	base := evalReport{Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.62}, PrecisionAt: map[int]float64{5: 0.5}}}
	now := evalReport{Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.71}, PrecisionAt: map[int]float64{5: 0.5}}}
	if regressed, _ := compareToBaseline(base, now); regressed {
		t.Error("an improvement was reported as a regression")
	}
}
