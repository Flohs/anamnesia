package store

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// twoProjects gives one user with a source and a target project.
func twoProjects(t *testing.T) (*Store, uuid.UUID, string, string) {
	t.Helper()
	st, scope := testStore(t)
	from := "move-from-" + uuid.NewString()[:8]
	to := "move-to-" + uuid.NewString()[:8]
	if _, err := st.EnsureProject(context.Background(), scope.UserID, from); err != nil {
		t.Fatalf("ensure from: %v", err)
	}
	return st, scope.UserID, from, to
}

func projectScope(t *testing.T, st *Store, uid uuid.UUID, slug string) anamnesia.Scope {
	t.Helper()
	pid, err := st.EnsureProject(context.Background(), uid, slug)
	if err != nil {
		t.Fatalf("ensure project %s: %v", slug, err)
	}
	return anamnesia.Scope{UserID: uid, ProjectID: &pid}
}

// TestTheMoverCoversEveryTableThatNamesAProject is the test that keeps
// this correct as the schema grows. A move that misses a table does not
// fail: it silently strands those rows under a project slug nothing files
// under any more, and they stop being retrievable in the project that now
// owns the work. Discovering the list from the database rather than
// repeating it means a new project-scoped table breaks this test on the
// migration that adds it, not months later.
func TestTheMoverCoversEveryTableThatNamesAProject(t *testing.T) {
	st, _ := testStore(t)
	rows, err := st.Pool.Query(context.Background(), `
		SELECT table_name FROM information_schema.columns
		WHERE table_schema='public' AND column_name='project_id'`)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	defer rows.Close()
	var inSchema []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		inSchema = append(inSchema, n)
	}
	covered := append([]string(nil), projectScopedTables...)
	sort.Strings(inSchema)
	sort.Strings(covered)
	if len(inSchema) != len(covered) {
		t.Fatalf("schema has project_id on %v, the mover covers %v", inSchema, covered)
	}
	for i := range inSchema {
		if inSchema[i] != covered[i] {
			t.Errorf("table %q carries project_id but the mover does not touch it", inSchema[i])
		}
	}
}

// TestAMoveCarriesTheRowsAcross is the basic promise.
func TestAMoveCarriesTheRowsAcross(t *testing.T) {
	st, uid, from, to := twoProjects(t)
	ctx := context.Background()
	src := projectScope(t, st, uid, from)

	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: src, FactKind: anamnesia.FactScopeProject,
		Key: "db.host", Value: map[string]any{"v": "localhost"},
	}); err != nil {
		t.Fatalf("fact: %v", err)
	}
	exp := &anamnesia.Experience{Scope: src, Kind: anamnesia.ExperienceCase, Title: "t", Body: "b"}
	if err := st.RecordExperience(ctx, exp); err != nil {
		t.Fatalf("experience: %v", err)
	}

	plan, err := st.PlanProjectMove(ctx, uid, from, to)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Counts["facts"] != 1 || plan.Counts["experiences"] != 1 {
		t.Fatalf("plan counts = %v, want one fact and one experience", plan.Counts)
	}
	if err := st.ApplyProjectMove(ctx, uid, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}

	dst := projectScope(t, st, uid, to)
	var facts, exps int
	if err := st.Pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM facts WHERE project_id=$1),
		        (SELECT count(*) FROM experiences WHERE project_id=$1)`,
		*dst.ProjectID).Scan(&facts, &exps); err != nil {
		t.Fatalf("count: %v", err)
	}
	if facts != 1 || exps != 1 {
		t.Errorf("after the move the target holds %d facts and %d experiences, want 1 and 1", facts, exps)
	}
	var left int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM facts WHERE project_id=$1`, *src.ProjectID).Scan(&left); err != nil {
		t.Fatalf("count source: %v", err)
	}
	if left != 0 {
		t.Errorf("%d rows still sit under the old project", left)
	}
}

// TestPlanningMovesNothing: the dry run is the whole safety story, so it
// must not be the thing that mutates.
func TestPlanningMovesNothing(t *testing.T) {
	st, uid, from, to := twoProjects(t)
	ctx := context.Background()
	src := projectScope(t, st, uid, from)
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: src, FactKind: anamnesia.FactScopeProject,
		Key: "db.host", Value: map[string]any{"v": "localhost"},
	}); err != nil {
		t.Fatalf("fact: %v", err)
	}
	if _, err := st.PlanProjectMove(ctx, uid, from, to); err != nil {
		t.Fatalf("plan: %v", err)
	}
	var still int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM facts WHERE project_id=$1`, *src.ProjectID).Scan(&still); err != nil {
		t.Fatal(err)
	}
	if still != 1 {
		t.Errorf("planning moved %d rows; a dry run must not write", 1-still)
	}
}

// TestAFactKeyCollisionBlocksTheMove. facts_identity is unique on
// (user, project, fact_scope, key), so two projects each holding
// "db.host" cannot both land in the target. Moving anyway would abort on
// the index; picking a winner silently would discard a fact the user
// never asked to lose.
func TestAFactKeyCollisionBlocksTheMove(t *testing.T) {
	st, uid, from, to := twoProjects(t)
	ctx := context.Background()
	src := projectScope(t, st, uid, from)
	dst := projectScope(t, st, uid, to)
	for _, sc := range []anamnesia.Scope{src, dst} {
		if err := st.UpsertFact(ctx, &anamnesia.Fact{
			Scope: sc, FactKind: anamnesia.FactScopeProject,
			Key: "db.host", Value: map[string]any{"v": "localhost"},
		}); err != nil {
			t.Fatalf("fact: %v", err)
		}
	}
	plan, err := st.PlanProjectMove(ctx, uid, from, to)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.FactKeys) != 1 || plan.FactKeys[0] != "db.host" {
		t.Errorf("plan.FactKeys = %v, want the colliding key named", plan.FactKeys)
	}
	if len(plan.Blockers()) == 0 {
		t.Fatal("a colliding key did not block the move")
	}
	if err := st.ApplyProjectMove(ctx, uid, plan); err == nil {
		t.Fatal("apply went ahead despite a collision")
	}
	var left int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM facts WHERE project_id=$1`, *src.ProjectID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Errorf("a refused move still moved rows: %d left under the source", left)
	}
}

// TestEntitiesBlockTheMove. entities_identity is unique on (user,
// project, kind, name) and, unlike the others, has no deleted_at
// predicate. Merging two entities means repointing every edge at the
// survivor, which is a different piece of work; until it exists the
// command has to refuse rather than half-do it.
func TestEntitiesBlockTheMove(t *testing.T) {
	st, uid, from, to := twoProjects(t)
	ctx := context.Background()
	src := projectScope(t, st, uid, from)
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO entities (user_id, project_id, kind, name) VALUES ($1,$2,'person','ada')`,
		uid, *src.ProjectID); err != nil {
		t.Fatalf("entity: %v", err)
	}
	plan, err := st.PlanProjectMove(ctx, uid, from, to)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Entities != 1 {
		t.Errorf("plan.Entities = %d, want 1", plan.Entities)
	}
	if len(plan.Blockers()) == 0 {
		t.Error("entities in the source did not block the move")
	}
}

// TestMovingToANewProjectCreatesIt: the target is usually a slug that
// does not exist yet, which is the whole point of the command.
func TestMovingToANewProjectCreatesIt(t *testing.T) {
	st, uid, from, to := twoProjects(t)
	ctx := context.Background()
	src := projectScope(t, st, uid, from)
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: src, FactKind: anamnesia.FactScopeProject,
		Key: "db.host", Value: map[string]any{"v": "localhost"},
	}); err != nil {
		t.Fatalf("fact: %v", err)
	}
	plan, err := st.PlanProjectMove(ctx, uid, from, to)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := st.ApplyProjectMove(ctx, uid, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, found, err := st.LookupProject(ctx, uid, to); err != nil || !found {
		t.Errorf("target project was not created: found=%v err=%v", found, err)
	}
}
