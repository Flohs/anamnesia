package jobs

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// vecAtAngle returns a 1536-wide unit vector whose cosine against
// [1,0,0,...] is exactly `sim`. Two coordinates are enough: the rest of
// the dimensions stay zero and contribute nothing to either norm.
func vecAtAngle(sim float64) []float32 {
	v := make([]float32, 1536)
	v[0] = float32(sim)
	v[1] = float32(math.Sqrt(1 - sim*sim))
	return v
}

func unitVec() []float32 {
	v := make([]float32, 1536)
	v[0] = 1
	return v
}

// TestTheDefaultThresholdIsReachableByRealContent is the bug that made
// consolidation dead code.
//
// The threshold shipped at 0.85. Measured on a real install on
// 2026-08-21, the most similar pair of experiences the user owned scored
// 0.754, across 1,402 same-scope pairs; the mean was 0.289 and NOTHING
// cleared 0.85. So no cluster of two could ever form, the LLM was never
// called, and every pass finished in ~42ms reporting success while
// folding nothing. It only ever produced output for the `doctor` health
// check, whose rows are byte-identical and score 1.000.
//
// A default a real corpus cannot reach is not a conservative default,
// it is an off switch that reports itself as healthy.
func TestTheDefaultThresholdIsReachableByRealContent(t *testing.T) {
	cfg := applyConsolidateDefaults(ConsolidateConfig{})

	const observedBest = 0.754
	a := &anamnesia.Experience{Embedding: unitVec()}
	b := &anamnesia.Experience{Embedding: vecAtAngle(observedBest)}

	clusters := buildClusters([]*anamnesia.Experience{a, b}, cfg.SimThreshold, cfg.MaxCluster)
	if len(clusters) != 1 || len(clusters[0].members) != 2 {
		t.Fatalf("the two most similar experiences on a real install (cosine %.3f) did not cluster at the default threshold %.2f: got %d clusters. Nothing in that corpus is more alike than this pair, so consolidation can never run.",
			observedBest, cfg.SimThreshold, len(clusters))
	}
}

// TestUnrelatedExperiencesStillDoNotCluster guards the over-correction.
// Lowering the threshold until everything merges would be worse than
// merging nothing: the mean real pair scored 0.289.
func TestUnrelatedExperiencesStillDoNotCluster(t *testing.T) {
	cfg := applyConsolidateDefaults(ConsolidateConfig{})

	a := &anamnesia.Experience{Embedding: unitVec()}
	b := &anamnesia.Experience{Embedding: vecAtAngle(0.289)}

	clusters := buildClusters([]*anamnesia.Experience{a, b}, cfg.SimThreshold, cfg.MaxCluster)
	if len(clusters) != 2 {
		t.Errorf("two unrelated experiences (cosine 0.289, the real mean) merged at threshold %.2f", cfg.SimThreshold)
	}
}

// consolidateFixture writes n identical-direction experiences in a fresh
// scope, so they are guaranteed to form one cluster.
func consolidateFixture(t *testing.T, n int) (*store.Store, anamnesia.Scope) {
	t.Helper()
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	uid, err := st.EnsureUser(ctx, "idem-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	pid, err := st.EnsureProject(ctx, uid, "idem-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	scope := anamnesia.Scope{UserID: uid, ProjectID: &pid}
	for i := 0; i < n; i++ {
		exp := &anamnesia.Experience{
			Scope: scope, Kind: anamnesia.ExperienceCase,
			Title: "clusterable " + uuid.NewString()[:6], Body: "body",
		}
		if err := st.RecordExperience(ctx, exp); err != nil {
			t.Fatalf("record: %v", err)
		}
		if err := st.SetExperienceEmbedding(ctx, exp.ID, unitVec(), "test"); err != nil {
			t.Fatalf("embed: %v", err)
		}
	}
	return st, scope
}

func abstractionOneCount(t *testing.T, st *store.Store, scope anamnesia.Scope) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM experiences
		 WHERE user_id=$1 AND project_id=$2 AND abstraction=1 AND deleted_at IS NULL`,
		scope.UserID, scope.ProjectID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestASecondPassDoesNotRedistilTheSameCluster is the other half of the
// bug. Consolidation is deliberately additive: it does NOT supersede its
// sources, because doing that once invalidated every source row and
// silently broke fact-grounded retrieval. But nothing replaced that
// guard, so the sources stay eligible forever and every pass distils the
// same cluster again.
//
// Measured on the same real install: 8 identical `doctor` rows had
// produced 13 summaries across 63 source links, and would have kept
// growing on every restart. Once the threshold is reachable this stops
// being a health-check curiosity and starts duplicating real memory,
// paying for an LLM call each time.
func TestASecondPassDoesNotRedistilTheSameCluster(t *testing.T) {
	st, scope := consolidateFixture(t, 3)
	ctx := context.Background()
	lm := &fakeDistiller{}

	run := func() {
		if err := ConsolidationRun(ctx, st, lm, ConsolidateConfig{}, discardLog(),
			7*24*time.Hour, activity.New(4)); err != nil {
			t.Fatalf("consolidation: %v", err)
		}
	}

	run()
	afterFirst := abstractionOneCount(t, st, scope)
	if afterFirst != 1 {
		t.Fatalf("first pass wrote %d summaries, want exactly 1", afterFirst)
	}
	callsAfterFirst := lm.calls

	run()
	if got := abstractionOneCount(t, st, scope); got != afterFirst {
		t.Errorf("second pass over unchanged data wrote another summary: %d then %d. Every restart would add one more.", afterFirst, got)
	}
	if lm.calls != callsAfterFirst {
		t.Errorf("the distiller was called again for an already-distilled cluster: %d then %d calls", callsAfterFirst, lm.calls)
	}
}

// TestANewExperienceStillTriggersAFreshDistillation guards the
// over-correction: the guard must key on the cluster's membership, not
// simply "this scope already has a summary".
func TestANewExperienceStillTriggersAFreshDistillation(t *testing.T) {
	st, scope := consolidateFixture(t, 3)
	ctx := context.Background()
	lm := &fakeDistiller{}

	run := func() {
		if err := ConsolidationRun(ctx, st, lm, ConsolidateConfig{}, discardLog(),
			7*24*time.Hour, activity.New(4)); err != nil {
			t.Fatalf("consolidation: %v", err)
		}
	}
	run()
	before := abstractionOneCount(t, st, scope)

	exp := &anamnesia.Experience{
		Scope: scope, Kind: anamnesia.ExperienceCase, Title: "a fourth", Body: "body",
	}
	if err := st.RecordExperience(ctx, exp); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := st.SetExperienceEmbedding(ctx, exp.ID, unitVec(), "test"); err != nil {
		t.Fatalf("embed: %v", err)
	}

	run()
	if got := abstractionOneCount(t, st, scope); got != before+1 {
		t.Errorf("summaries = %d, want %d: a cluster that gained a member is not the cluster that was already distilled", got, before+1)
	}
}
