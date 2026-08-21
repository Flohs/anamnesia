package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// sourceAt inserts a source whose content is dated `at`.
func sourceAt(t *testing.T, st *Store, scope anamnesia.Scope, at time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := st.Pool.QueryRow(context.Background(), `
		INSERT INTO sources (user_id, project_id, kind, occurred_at)
		VALUES ($1,$2,'conversation',$3) RETURNING id`,
		scope.UserID, scope.ProjectID, at).Scan(&id); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	return id
}

func currentValue(t *testing.T, st *Store, scope anamnesia.Scope, key string) string {
	t.Helper()
	var v string
	if err := st.Pool.QueryRow(context.Background(), `
		SELECT value->>'v' FROM facts
		 WHERE user_id=$1 AND key=$2 AND deleted_at IS NULL AND superseded_by IS NULL`,
		scope.UserID, key).Scan(&v); err != nil {
		t.Fatalf("read current: %v", err)
	}
	return v
}

func factFrom(scope anamnesia.Scope, key, val string, src uuid.UUID) *anamnesia.Fact {
	f := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeProject,
		Key: key, Value: map[string]any{"v": val}, Source: "extracted",
	}
	if src != uuid.Nil {
		f.SourceID = &src
	}
	return f
}

// TestAnOlderSourceDoesNotOverwriteANewerValue is a defect observed in
// live memory, not a hypothetical.
//
// On one install `project.schema_version` read v10, then four seconds
// later was superseded by v8. The schema was, and is, v10. The same key
// took `project.current_release` from rc5 backwards to rc4. Nothing was
// corrupt: the extractor applies UPDATE_FACT in whatever order sources
// happen to be processed, and with segmentation and extract_concurrency
// that order is not even deterministic. Whichever assertion lands last
// wins, regardless of which one describes a later moment.
//
// Fact history recorded all of it faithfully, which is how the
// regression was visible at all.
func TestAnOlderSourceDoesNotOverwriteANewerValue(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	newer := sourceAt(t, st, scope, now)
	older := sourceAt(t, st, scope, now.Add(-2*time.Hour))

	if err := st.UpsertFact(ctx, factFrom(scope, "project.schema_version", "v10", newer)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	err := st.UpsertFact(ctx, factFrom(scope, "project.schema_version", "v8", older))
	if err == nil {
		t.Fatal("an assertion from two-hour-old content overwrote a newer one without complaint")
	}
	if !errors.Is(err, ErrStaleAssertion) {
		t.Errorf("err = %v, want ErrStaleAssertion so a caller can tell a refusal from a failure", err)
	}
	if got := currentValue(t, st, scope, "project.schema_version"); got != "v10" {
		t.Errorf("current value = %q, want v10: the newer assertion must stay current", got)
	}
}

// TestANewerSourceStillOverwrites guards the over-correction. A guard
// that refused every change would freeze memory at its first value.
func TestANewerSourceStillOverwrites(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	older := sourceAt(t, st, scope, now.Add(-2*time.Hour))
	newer := sourceAt(t, st, scope, now)

	if err := st.UpsertFact(ctx, factFrom(scope, "project.release", "rc7", older)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := st.UpsertFact(ctx, factFrom(scope, "project.release", "rc11", newer)); err != nil {
		t.Fatalf("newer write was refused: %v", err)
	}
	if got := currentValue(t, st, scope, "project.release"); got != "rc11" {
		t.Errorf("current value = %q, want rc11", got)
	}
}

// TestAFactWithoutASourceIsNeverRefused. A person setting a value through
// anamnesia_facts_upsert or the CLI has no source row and is stating what
// is true now. They must always win; the guard is about two extractions
// disagreeing, not about overruling the user.
func TestAFactWithoutASourceIsNeverRefused(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	newer := sourceAt(t, st, scope, now)
	if err := st.UpsertFact(ctx, factFrom(scope, "user.editor", "vim", newer)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := st.UpsertFact(ctx, factFrom(scope, "user.editor", "helix", uuid.Nil)); err != nil {
		t.Fatalf("a sourceless write was refused: %v", err)
	}
	if got := currentValue(t, st, scope, "user.editor"); got != "helix" {
		t.Errorf("current value = %q, want helix", got)
	}
}

// TestAFactWhoseCurrentRowHasNoSourceIsStillUpdatable: rows written
// before source_id was recorded, and rows written by hand, must not
// become permanently unchangeable by extraction.
func TestAFactWhoseCurrentRowHasNoSourceIsStillUpdatable(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	if err := st.UpsertFact(ctx, factFrom(scope, "user.shell", "bash", uuid.Nil)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	old := sourceAt(t, st, scope, time.Now().UTC().Add(-72*time.Hour))
	if err := st.UpsertFact(ctx, factFrom(scope, "user.shell", "zsh", old)); err != nil {
		t.Fatalf("update against a sourceless row was refused: %v", err)
	}
	if got := currentValue(t, st, scope, "user.shell"); got != "zsh" {
		t.Errorf("current value = %q, want zsh", got)
	}
}

// TestReassertingTheSameValueFromOldContentIsFine: the extractor
// re-asserts constantly, and the same value from an older source is not a
// regression, it is agreement. Refusing it would turn ordinary repetition
// into a stream of errors in the trace.
func TestReassertingTheSameValueFromOldContentIsFine(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	newer := sourceAt(t, st, scope, now)
	older := sourceAt(t, st, scope, now.Add(-time.Hour))
	if err := st.UpsertFact(ctx, factFrom(scope, "project.lang", "go", newer)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := st.UpsertFact(ctx, factFrom(scope, "project.lang", "go", older)); err != nil {
		t.Errorf("re-asserting the same value from older content was refused: %v", err)
	}
}
