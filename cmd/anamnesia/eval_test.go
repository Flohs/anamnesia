package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestJSONRunSendsOnlyTheReportToStdout(t *testing.T) {
	// The bug this guards against: `eval --json --baseline old.json >
	// new.json` is the natural CI invocation (capture the artifact and
	// gate in one call), and anything besides the report reaching out
	// makes the redirected file invalid JSON.
	path := filepath.Join(t.TempDir(), "baseline.json")
	base := evalReport{Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.9}, PrecisionAt: map[int]float64{5: 0.5}}}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	now := evalReport{Queries: 1, Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.9}, PrecisionAt: map[int]float64{5: 0.5}}}
	var out, errOut strings.Builder
	if err := writeEvalResult(&out, &errOut, true, now, path); err != nil {
		t.Fatalf("writeEvalResult: %v", err)
	}

	var decoded evalReport
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Errorf("stdout was not valid JSON: %v\nstdout:\n%s", err, out.String())
	}
	if errOut.Len() == 0 {
		t.Error("the baseline comparison did not reach stderr")
	}
}
