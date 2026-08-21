package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/flohs/anamnesia/internal/jobs"
)

// TestConsolidationSettingsReachTheServer: the clustering threshold was
// hardcoded, so the only way to change how consolidation behaves was to
// rebuild. That mattered because the compiled-in value was one no real
// corpus could reach, and there was no way to find that out or correct it
// from the outside.
func TestConsolidationSettingsReachTheServer(t *testing.T) {
	isolatedHome(t)
	hc, err := loadHostConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := hc.Get("worker.consolidate_similarity"); got != "0.65" {
		t.Errorf("worker.consolidate_similarity default = %q, want 0.65", got)
	}
	if got := hc.Get("worker.consolidate_max_cluster"); got != "8" {
		t.Errorf("worker.consolidate_max_cluster default = %q, want 8", got)
	}
	env := strings.Join(hc.ServerEnv(), "\n")
	for _, want := range []string{
		"ANAMNESIA_CONSOLIDATE_SIMILARITY=0.65",
		"ANAMNESIA_CONSOLIDATE_MAX_CLUSTER=8",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("server environment is missing %s", want)
		}
	}
}

// TestConsolidateDefaultsAgreeWithTheClusterer pins the two ends of the
// path together, the same way embed.dims and the schema width have to
// agree. settings.go is what a generated config file and `anamnesia
// config` show; applyConsolidateDefaults is what a caller that supplies
// no config gets. If they drift, the documented default stops being the
// one that runs and the disagreement is invisible.
func TestConsolidateDefaultsAgreeWithTheClusterer(t *testing.T) {
	sim := strconv.FormatFloat(jobs.DefaultConsolidateSimilarity, 'g', -1, 64)
	if got := settingByKey["worker.consolidate_similarity"].Def; got != sim {
		t.Errorf("declared default %q, clusterer uses %q", got, sim)
	}
	maxc := strconv.Itoa(jobs.DefaultConsolidateMaxCluster)
	if got := settingByKey["worker.consolidate_max_cluster"].Def; got != maxc {
		t.Errorf("declared default %q, clusterer uses %q", got, maxc)
	}
}

// TestConsolidateSimilarityIsBoundedWhereItIsTyped: a threshold above 1
// is unreachable by any cosine and silently switches consolidation off,
// which is exactly the failure being fixed here. It has to be rejected at
// the point of entry, not accepted into the file and discovered later.
func TestConsolidateSimilarityIsBoundedWhereItIsTyped(t *testing.T) {
	for _, bad := range []string{"1.5", "-0.1", "NaN", "high"} {
		if _, err := settingByKey["worker.consolidate_similarity"].validate(bad); err == nil {
			t.Errorf("worker.consolidate_similarity accepted %q", bad)
		}
	}
	for _, good := range []string{"0.65", "0", "1"} {
		if _, err := settingByKey["worker.consolidate_similarity"].validate(good); err != nil {
			t.Errorf("worker.consolidate_similarity rejected %q: %v", good, err)
		}
	}
}
