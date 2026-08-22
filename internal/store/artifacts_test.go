package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

func newArtifact(scope anamnesia.Scope, id uuid.UUID) *anamnesia.Artifact {
	return &anamnesia.Artifact{
		Scope:        scope,
		ArtifactUUID: id,
		URL:          "https://claude.ai/code/artifact/" + id.String(),
		Description:  "An audit of what anamnesia writes",
		FilePath:     "/tmp/scratch/audit.html",
		Body:         "six of fifty-four session-end hooks failed",
		OccurredAt:   time.Now().UTC().Add(-time.Hour),
	}
}

// TestRepublishUpdatesTheSameRow is the property the whole identity choice
// rests on: an artifact redeploys to the same URL, so publishing twice is
// one artifact, not two.
func TestRepublishUpdatesTheSameRow(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	id := uuid.New()

	first := newArtifact(scope, id)
	if err := st.UpsertArtifact(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := newArtifact(scope, id)
	second.Description = "An audit, revised"
	second.Body = "thirty-one artifacts across nine projects"
	if err := st.UpsertArtifact(ctx, second); err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Errorf("republish created a new row: %s then %s", first.ID, second.ID)
	}
	got, err := st.ListArtifacts(ctx, scope, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 artifact after republishing, got %d", len(got))
	}
	if got[0].Description != "An audit, revised" {
		t.Errorf("description = %q, want the republished one", got[0].Description)
	}
}

// A changed body has to be re-embedded, or retrieval keeps matching the
// text the artifact used to hold.
func TestChangedBodyClearsTheEmbedding(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	id := uuid.New()

	a := newArtifact(scope, id)
	if err := st.UpsertArtifact(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := st.SetArtifactEmbedding(ctx, a.ID, make([]float32, 1536), "stub"); err != nil {
		t.Fatal(err)
	}
	// Read the row rather than the worker's queue: that queue is global
	// and capped, so every other test's unembedded artifact competes for
	// the same window and the assertion would pass or fail on ordering.
	if model := embedModelOf(t, st, scope, id); model != "stub" {
		t.Fatalf("embed_model = %q after embedding, want stub", model)
	}

	changed := newArtifact(scope, id)
	changed.Body = "an entirely different page"
	if err := st.UpsertArtifact(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if model := embedModelOf(t, st, scope, id); model != "" {
		t.Errorf("embed_model = %q after the body changed, want it cleared for re-embedding", model)
	}
}

// embedModelOf reads one artifact's embed_model, which is cleared with
// its embedding and so says whether it is queued for re-embedding.
func embedModelOf(t *testing.T, st *Store, scope anamnesia.Scope, artifactUUID uuid.UUID) string {
	t.Helper()
	var model *string
	err := st.Pool.QueryRow(context.Background(),
		`SELECT embed_model FROM artifacts WHERE user_id = $1 AND artifact_uuid = $2`,
		scope.UserID, artifactUUID).Scan(&model)
	if err != nil {
		t.Fatal(err)
	}
	if model == nil {
		return ""
	}
	return *model
}

// The backfill reads transcripts long after the scratchpad was cleaned
// up, so it usually has no body. Running it must not erase one the live
// hook captured while the file still existed.
func TestABodylessUpsertKeepsTheStoredBody(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	id := uuid.New()

	live := newArtifact(scope, id)
	if err := st.UpsertArtifact(ctx, live); err != nil {
		t.Fatal(err)
	}
	backfilled := newArtifact(scope, id)
	backfilled.Body = ""
	backfilled.FilePath = ""
	if err := st.UpsertArtifact(ctx, backfilled); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListArtifacts(ctx, scope, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != live.Body {
		t.Errorf("body = %q, want the captured one %q", got[0].Body, live.Body)
	}
	if got[0].FilePath != live.FilePath {
		t.Errorf("file_path = %q, want it kept", got[0].FilePath)
	}
}

// An artifact belongs to the work that made it. Republishing from another
// repository must not refile it, or a shared page drifts to whichever
// project touched it last.
func TestRepublishDoesNotMoveTheProject(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	id := uuid.New()

	if err := st.UpsertArtifact(ctx, newArtifact(scope, id)); err != nil {
		t.Fatal(err)
	}
	other, err := st.EnsureProject(ctx, scope.UserID, "somewhere-else")
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := newArtifact(anamnesia.Scope{UserID: scope.UserID, ProjectID: &other}, id)
	if err := st.UpsertArtifact(ctx, elsewhere); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListArtifacts(ctx, anamnesia.Scope{UserID: scope.UserID}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(got))
	}
	if got[0].Scope.ProjectID == nil || *got[0].Scope.ProjectID != *scope.ProjectID {
		t.Errorf("project moved to %v, want it to stay at %v", got[0].Scope.ProjectID, scope.ProjectID)
	}
}
