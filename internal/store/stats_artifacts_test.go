package store

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// TestEmbeddingCoverageReportsEveryEmbeddingTable keeps the stats
// endpoint honest as domains are added.
//
// Coverage is what a person reads to answer "is my memory searchable
// yet". A table missing from it does not read as missing: it reads as
// everything being covered, because the domains that are listed are all
// at 100%. Deriving the expectation from embeddingTables means a new
// embedding domain fails here on the change that adds it.
func TestEmbeddingCoverageReportsEveryEmbeddingTable(t *testing.T) {
	st, scope := testStore(t)

	res, err := st.Stats(context.Background(), scope)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	var reported []string
	for domain := range res.EmbeddingCoverage {
		reported = append(reported, domain)
	}
	want := append([]string(nil), embeddingTables...)
	sort.Strings(reported)
	sort.Strings(want)
	if strings.Join(reported, ",") != strings.Join(want, ",") {
		t.Errorf("embedding coverage reports %v, but the schema embeds %v.\n"+
			"A domain missing here reads as fully covered rather than as missing.",
			reported, want)
	}
}

// Artifacts have to appear in the totals too, or `anamnesia status`
// under-reports what memory holds.
func TestStatsCountArtifacts(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	before, err := st.Stats(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if err := st.UpsertArtifact(ctx, &anamnesia.Artifact{
		Scope: scope, ArtifactUUID: id, URL: "https://claude.ai/code/artifact/" + id.String(),
		Description: "a counted artifact",
	}); err != nil {
		t.Fatal(err)
	}
	after, err := st.Stats(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if after.Artifacts != before.Artifacts+1 {
		t.Errorf("artifacts total went %d → %d, want one more", before.Artifacts, after.Artifacts)
	}
	if cov, ok := after.EmbeddingCoverage["artifacts"]; !ok || cov.Total != after.Artifacts {
		t.Errorf("artifact coverage = %+v, want a total of %d", cov, after.Artifacts)
	}
}
