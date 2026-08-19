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
	regressed, lines, err := compareToBaseline(base, now)
	if err != nil {
		t.Fatalf("compareToBaseline: %v", err)
	}
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
	regressed, _, err := compareToBaseline(base, now)
	if err != nil {
		t.Fatalf("compareToBaseline: %v", err)
	}
	if regressed {
		t.Error("an improvement was reported as a regression")
	}
}

func TestBaselineComparisonRefusesADifferentK(t *testing.T) {
	// A baseline taken with a different --k requested a different number
	// of hits per query, so its recall@k values answer a different
	// question than the current run's. Comparing them metric-by-metric
	// would silently report on numbers that were never asking the same
	// thing.
	base := evalReport{K: 10, Queries: 3, Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.8, 10: 0.9}, PrecisionAt: map[int]float64{5: 0.5, 10: 0.4}}}
	now := evalReport{K: 5, Queries: 3, Aggregate: aggregateScore{RecallAt: map[int]float64{1: 0.5, 5: 0.7}, PrecisionAt: map[int]float64{1: 0.5, 5: 0.4}}}
	_, _, err := compareToBaseline(base, now)
	if err == nil {
		t.Fatal("comparing runs with different K did not return an error")
	}
	if !strings.Contains(err.Error(), "10") || !strings.Contains(err.Error(), "5") {
		t.Errorf("error does not name both K values: %v", err)
	}
}

func TestBaselineComparisonRefusesADifferentQueryCount(t *testing.T) {
	base := evalReport{K: 5, Queries: 10, Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.8}, PrecisionAt: map[int]float64{5: 0.5}}}
	now := evalReport{K: 5, Queries: 12, Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.7}, PrecisionAt: map[int]float64{5: 0.5}}}
	_, _, err := compareToBaseline(base, now)
	if err == nil {
		t.Fatal("comparing runs with different query counts did not return an error")
	}
	if !strings.Contains(err.Error(), "10") || !strings.Contains(err.Error(), "12") {
		t.Errorf("error does not name both query counts: %v", err)
	}
}

func TestBaselineComparisonWithMatchingKStillCompares(t *testing.T) {
	base := evalReport{K: 5, Queries: 3, Aggregate: aggregateScore{RecallAt: map[int]float64{1: 0.5, 5: 0.80}, PrecisionAt: map[int]float64{1: 0.5, 5: 0.5}}}
	now := evalReport{K: 5, Queries: 3, Aggregate: aggregateScore{RecallAt: map[int]float64{1: 0.5, 5: 0.62}, PrecisionAt: map[int]float64{1: 0.5, 5: 0.5}}}
	regressed, lines, err := compareToBaseline(base, now)
	if err != nil {
		t.Fatalf("compareToBaseline: %v", err)
	}
	if !regressed {
		t.Error("a recall@5 drop from 0.80 to 0.62 was not reported as a regression")
	}
	if len(lines) == 0 {
		t.Error("comparison produced no explanation")
	}
}

func TestEvalMetricKsExcludesCutoffsAboveK(t *testing.T) {
	// `eval --k 5` must never label a metric "recall@10": only 5 hits
	// were ever requested, so nothing beyond @5 was measured.
	ks := evalMetricKs(5)
	for _, k := range ks {
		if k > 5 {
			t.Errorf("evalMetricKs(5) included cutoff %d, which exceeds k", k)
		}
	}
	found5 := false
	for _, k := range ks {
		if k == 5 {
			found5 = true
		}
	}
	if !found5 {
		t.Errorf("evalMetricKs(5) = %v, want it to include 5", ks)
	}
}

func TestEvalMetricKsKeepsKWhenBelowEveryStandardCutoff(t *testing.T) {
	ks := evalMetricKs(1)
	if len(ks) != 1 || ks[0] != 1 {
		t.Errorf("evalMetricKs(1) = %v, want [1]", ks)
	}
}

func TestRunEvalPassReportsK(t *testing.T) {
	// --k 5 must not silently produce a recall@10 key: the report has to
	// say what k was actually used, and the aggregate must not carry a
	// cutoff the run never requested.
	scores := []queryScore{score("q-001", []string{"a", "b"}, []string{"a"}, evalMetricKs(5), 10)}
	agg := aggregate(scores, evalMetricKs(5))
	report := evalReport{K: 5, Queries: 1, Aggregate: agg, PerQuery: scores}

	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["k"] != float64(5) {
		t.Errorf(`report json "k" = %v, want 5`, decoded["k"])
	}
	aggregateJSON, ok := decoded["aggregate"].(map[string]any)
	if !ok {
		t.Fatal("report json has no aggregate object")
	}
	recallAt, ok := aggregateJSON["RecallAt"].(map[string]any)
	if !ok {
		t.Fatal("aggregate json has no RecallAt object")
	}
	if _, present := recallAt["10"]; present {
		t.Errorf("a --k 5 run reported a recall@10 key: %v", recallAt)
	}
	if _, present := recallAt["5"]; !present {
		t.Errorf("a --k 5 run did not report recall@5: %v", recallAt)
	}
}

func TestJSONRunSendsOnlyTheReportToStdout(t *testing.T) {
	// The bug this guards against: `eval --json --baseline old.json >
	// new.json` is the natural CI invocation (capture the artifact and
	// gate in one call), and anything besides the report reaching out
	// makes the redirected file invalid JSON.
	path := filepath.Join(t.TempDir(), "baseline.json")
	base := evalReport{K: 5, Queries: 1, Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.9}, PrecisionAt: map[int]float64{5: 0.5}}}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	now := evalReport{K: 5, Queries: 1, Aggregate: aggregateScore{RecallAt: map[int]float64{5: 0.9}, PrecisionAt: map[int]float64{5: 0.5}}}
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

// A run is scored against whatever extraction actually produced, which is not
// necessarily what was ingested: MarkSkipped and MarkFailed both move a source
// out of 'pending', so the drain completes and the shortfall is invisible.
func TestReportShowsWhatTheCorpusActuallyBecame(t *testing.T) {
	r := evalReport{
		Queries: 25,
		Corpus: corpusStats{
			SourcesByState: map[string]int{"done": 34, "skipped": 4, "failed": 2},
			Facts:          16,
			Experiences:    30,
		},
		Aggregate: aggregateScore{Queries: 25, RecallAt: map[int]float64{5: 0.7}, PrecisionAt: map[int]float64{5: 0.2}},
	}
	var sb strings.Builder
	renderReport(&sb, r)
	out := sb.String()
	for _, want := range []string{"34", "skipped 4", "failed 2", "16 facts", "30 experiences"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not show %q:\n%s", want, out)
		}
	}
}

func TestReportWarnsWhenSourcesProducedNothing(t *testing.T) {
	// Six of forty sources produced no rows. The scored corpus is smaller than
	// the ingested one, and a reader comparing two runs has to be told.
	r := evalReport{
		Queries: 25,
		Corpus: corpusStats{
			SourcesByState: map[string]int{"done": 34, "skipped": 4, "failed": 2},
			Facts:          16, Experiences: 30,
		},
		Aggregate: aggregateScore{Queries: 25},
	}
	var sb strings.Builder
	renderReport(&sb, r)
	if !strings.Contains(sb.String(), "6 of 40") {
		t.Errorf("report does not warn that 6 of 40 sources produced nothing:\n%s", sb.String())
	}
}

func TestReportIsQuietWhenEverySourceExtracted(t *testing.T) {
	r := evalReport{
		Queries: 25,
		Corpus: corpusStats{
			SourcesByState: map[string]int{"done": 40},
			Facts:          22, Experiences: 41,
		},
		Aggregate: aggregateScore{Queries: 25},
	}
	var sb strings.Builder
	renderReport(&sb, r)
	if strings.Contains(sb.String(), "produced nothing") {
		t.Errorf("report warns when nothing was lost:\n%s", sb.String())
	}
}

func TestCorpusShortfallCounts(t *testing.T) {
	c := corpusStats{SourcesByState: map[string]int{"done": 34, "skipped": 4, "failed": 2}}
	if got, want := c.ingested(), 40; got != want {
		t.Errorf("ingested = %d, want %d", got, want)
	}
	if got, want := c.barren(), 6; got != want {
		t.Errorf("barren = %d, want %d (skipped + failed)", got, want)
	}
	empty := corpusStats{}
	if empty.ingested() != 0 || empty.barren() != 0 {
		t.Error("an empty corpusStats must report zeroes, not panic")
	}
}
