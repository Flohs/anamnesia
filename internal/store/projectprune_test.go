package store

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// TestPruneFindsOnlyProjectsHoldingNothing. The whole safety of this
// command is that "empty" means empty in every table that names a
// project, not just the two anyone thinks to check.
func TestPruneFindsOnlyProjectsHoldingNothing(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	uid := scope.UserID

	empty := "prune-empty-" + uuid.NewString()[:8]
	if _, err := st.EnsureProject(ctx, uid, empty); err != nil {
		t.Fatal(err)
	}
	full := "prune-full-" + uuid.NewString()[:8]
	fullScope := projectScope(t, st, uid, full)
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: fullScope, FactKind: anamnesia.FactScopeProject,
		Key: "k", Value: map[string]any{"v": "x"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.PrunableProjects(ctx, uid)
	if err != nil {
		t.Fatalf("prunable: %v", err)
	}
	slugs := map[string]bool{}
	for _, p := range got {
		slugs[p.Slug] = true
	}
	if !slugs[empty] {
		t.Errorf("an empty project was not offered for pruning: %v", slugs)
	}
	if slugs[full] {
		t.Error("a project holding a fact was offered for pruning")
	}
}

// TestEveryProjectScopedTableProtectsAProject: a table left out of the
// emptiness check is how this command deletes something it should not.
// Each one is exercised rather than assumed, and the list is the same one
// the mover uses, which is itself checked against the live schema.
func TestEveryProjectScopedTableProtectsAProject(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	uid := scope.UserID

	// Rows that satisfy each table's NOT NULLs, keyed by table.
	inserts := map[string]string{
		"facts":          `INSERT INTO facts (user_id, project_id, fact_scope, key, value) VALUES ($1,$2,'project','k','{}'::jsonb)`,
		"experiences":    `INSERT INTO experiences (user_id, project_id, kind, body) VALUES ($1,$2,'case','b')`,
		"sources":        `INSERT INTO sources (user_id, project_id, kind) VALUES ($1,$2,'conversation')`,
		"entities":       `INSERT INTO entities (user_id, project_id, kind, name) VALUES ($1,$2,'person','ada')`,
		"skills":         `INSERT INTO skills (user_id, project_id, name, kind) VALUES ($1,$2,'s','function')`,
		"working_memory": `INSERT INTO working_memory (user_id, project_id, session_id, position, role, body) VALUES ($1,$2,gen_random_uuid(),1,'observation','b')`,
		"commitments":    `INSERT INTO commitments (user_id, project_id, owner, beneficiary, body) VALUES ($1,$2,'me','you','b')`,
		"audit_log":      `INSERT INTO audit_log (user_id, project_id, op, target) VALUES ($1,$2,'op','t')`,
	}
	for _, tbl := range projectScopedTables {
		q, ok := inserts[tbl]
		if !ok {
			t.Fatalf("table %q carries project_id but this test has no row for it; prune's emptiness check is unverified for it", tbl)
		}
		slug := "prune-" + tbl + "-" + uuid.NewString()[:8]
		pid, err := st.EnsureProject(ctx, uid, slug)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool.Exec(ctx, q, uid, pid); err != nil {
			t.Fatalf("seed %s: %v", tbl, err)
		}
		got, err := st.PrunableProjects(ctx, uid)
		if err != nil {
			t.Fatalf("prunable: %v", err)
		}
		for _, p := range got {
			if p.Slug == slug {
				t.Errorf("a project holding a row in %s was offered for pruning", tbl)
			}
		}
	}
}

// TestPruningDeletesOnlyWhatItListed.
func TestPruningDeletesOnlyWhatItListed(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	uid := scope.UserID

	empty := "prune-go-" + uuid.NewString()[:8]
	if _, err := st.EnsureProject(ctx, uid, empty); err != nil {
		t.Fatal(err)
	}
	keep := "prune-keep-" + uuid.NewString()[:8]
	keepScope := projectScope(t, st, uid, keep)
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: keepScope, FactKind: anamnesia.FactScopeProject,
		Key: "k", Value: map[string]any{"v": "x"},
	}); err != nil {
		t.Fatal(err)
	}

	n, err := st.PruneProjects(ctx, uid, []string{empty})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	if _, found, _ := st.LookupProject(ctx, uid, empty); found {
		t.Error("the empty project survived")
	}
	if _, found, _ := st.LookupProject(ctx, uid, keep); !found {
		t.Error("a project that was not listed was deleted")
	}
}

// TestPruningRefusesAProjectThatGainedRows closes the window between the
// dry run and the apply: the list is a snapshot, and a session can write
// into one of those projects in between. Re-checking at delete time is
// what makes the two-step safe rather than merely reassuring.
func TestPruningRefusesAProjectThatGainedRows(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	uid := scope.UserID

	slug := "prune-race-" + uuid.NewString()[:8]
	raceScope := projectScope(t, st, uid, slug)
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: raceScope, FactKind: anamnesia.FactScopeProject,
		Key: "k", Value: map[string]any{"v": "x"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := st.PruneProjects(ctx, uid, []string{slug}); err == nil {
		t.Fatal("prune deleted a project that holds a fact")
	}
	if _, found, _ := st.LookupProject(ctx, uid, slug); !found {
		t.Error("the project was deleted despite the refusal")
	}
}
