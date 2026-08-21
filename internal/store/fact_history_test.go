package store

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// currentFact reads the one live row for a key, and how many rows exist
// for it in total.
func currentFact(t *testing.T, st *Store, scope anamnesia.Scope, key string) (id uuid.UUID, value string, versions int) {
	t.Helper()
	ctx := context.Background()
	if err := st.Pool.QueryRow(ctx, `
		SELECT id, value->>'v' FROM facts
		WHERE user_id = $1 AND key = $2 AND deleted_at IS NULL AND superseded_by IS NULL`,
		scope.UserID, key).Scan(&id, &value); err != nil {
		t.Fatalf("read current %q: %v", key, err)
	}
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM facts WHERE user_id = $1 AND key = $2`,
		scope.UserID, key).Scan(&versions); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	return id, value, versions
}

func upsert(t *testing.T, st *Store, scope anamnesia.Scope, key, v string, src *uuid.UUID) *anamnesia.Fact {
	t.Helper()
	f := &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeUser, Key: key,
		Value: map[string]any{"v": v}, SourceID: src,
	}
	if err := st.UpsertFact(context.Background(), f); err != nil {
		t.Fatalf("upsert %q=%q: %v", key, v, err)
	}
	return f
}

// TestAChangedValueSupersedesRatherThanOverwrites is the whole point:
// the old value has to survive somewhere.
func TestAChangedValueSupersedesRatherThanOverwrites(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	first := upsert(t, st, scope, "user.commute", "cycles to work", nil)
	second := upsert(t, st, scope, "user.commute", "takes the tram", nil)

	cur, val, versions := currentFact(t, st, scope, "user.commute")
	if versions != 2 {
		t.Fatalf("versions = %d, want 2: the old value was overwritten, not superseded", versions)
	}
	if cur != second.ID || val != "takes the tram" {
		t.Errorf("current row = %v %q, want the new value", cur, val)
	}

	var supersededBy *uuid.UUID
	var validTo, invalidatedAt *string
	if err := st.Pool.QueryRow(ctx,
		`SELECT superseded_by, valid_to::text, invalidated_at::text FROM facts WHERE id = $1`,
		first.ID).Scan(&supersededBy, &validTo, &invalidatedAt); err != nil {
		t.Fatalf("read superseded row: %v", err)
	}
	if supersededBy == nil || *supersededBy != second.ID {
		t.Errorf("superseded_by = %v, want the replacement's id: history with no forward link cannot be walked", supersededBy)
	}
	if validTo == nil {
		t.Error("valid_to is NULL: nothing records when the old value stopped being true")
	}
	if invalidatedAt == nil {
		t.Error("invalidated_at is NULL")
	}
}

func TestTheSupersededRowKeepsItsOwnValue(t *testing.T) {
	st, scope := testStore(t)
	first := upsert(t, st, scope, "user.commute", "cycles to work", nil)
	upsert(t, st, scope, "user.commute", "takes the tram", nil)

	var old string
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT value->>'v' FROM facts WHERE id = $1`, first.ID).Scan(&old); err != nil {
		t.Fatalf("read: %v", err)
	}
	if old != "cycles to work" {
		t.Errorf("superseded value = %q, want the value it held: a history of the current value is not a history", old)
	}
}

func TestAnUnchangedValueCreatesNoVersion(t *testing.T) {
	// The extractor re-asserts facts constantly. A version per mention
	// would bury the real changes.
	st, scope := testStore(t)
	upsert(t, st, scope, "user.commute", "cycles to work", nil)
	upsert(t, st, scope, "user.commute", "cycles to work", nil)
	upsert(t, st, scope, "user.commute", "cycles to work", nil)

	if _, _, versions := currentFact(t, st, scope, "user.commute"); versions != 1 {
		t.Errorf("versions = %d, want 1: re-asserting a value is not changing it", versions)
	}
}

func TestSupersededRowsKeepTheirOwnProvenance(t *testing.T) {
	// Each version is evidence of what a particular source said. Moving
	// the old row's source_id to the new writer would destroy that.
	st, scope := testStore(t)
	ctx := context.Background()
	srcA := &anamnesia.Source{Scope: scope, Kind: "chat-turn", RawContent: "I cycle to work."}
	srcB := &anamnesia.Source{Scope: scope, Kind: "chat-turn", RawContent: "I take the tram now."}
	for _, s := range []*anamnesia.Source{srcA, srcB} {
		if err := st.InsertSource(ctx, s); err != nil {
			t.Fatalf("insert source: %v", err)
		}
	}
	first := upsert(t, st, scope, "user.commute", "cycles to work", &srcA.ID)
	second := upsert(t, st, scope, "user.commute", "takes the tram", &srcB.ID)

	for _, c := range []struct {
		id   uuid.UUID
		want uuid.UUID
		what string
	}{{first.ID, srcA.ID, "superseded"}, {second.ID, srcB.ID, "current"}} {
		var got uuid.UUID
		if err := st.Pool.QueryRow(ctx, `SELECT source_id FROM facts WHERE id = $1`, c.id).Scan(&got); err != nil {
			t.Fatalf("read %s source: %v", c.what, err)
		}
		if got != c.want {
			t.Errorf("%s row source_id = %v, want %v", c.what, got, c.want)
		}
	}
}

func TestGetFactAndListFactsSeeOnlyCurrentValues(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	upsert(t, st, scope, "user.commute", "cycles to work", nil)
	upsert(t, st, scope, "user.commute", "takes the tram", nil)

	facts, err := st.ListFacts(ctx, scope, anamnesia.FactScopeUser, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	n := 0
	for _, f := range facts {
		if f.Key == "user.commute" {
			n++
			if v, _ := f.Value["v"].(string); v != "takes the tram" {
				t.Errorf("listed value = %q, want the current one", v)
			}
		}
	}
	if n != 1 {
		t.Errorf("ListFacts returned %d rows for one key: history leaked into an ordinary read", n)
	}
}

func TestTwoCurrentRowsForOneKeyAreImpossible(t *testing.T) {
	// The partial unique index is the backstop behind the FOR UPDATE
	// lock. Without it a lost race silently forks a fact in two.
	st, scope := testStore(t)
	ctx := context.Background()
	f := upsert(t, st, scope, "user.commute", "cycles to work", nil)

	_, err := st.Pool.Exec(ctx, `
		INSERT INTO facts (user_id, project_id, fact_scope, key, value, source, trust, pii_tags, valid_from, ingested_at)
		SELECT user_id, project_id, fact_scope, key, '{"v":"forked"}'::jsonb, source, trust, pii_tags, now(), now()
		FROM facts WHERE id = $1`, f.ID)
	if err == nil {
		t.Error("a second current row for one key was accepted: the uniqueness contract is gone")
	}
}

func TestConcurrentUpsertsLeaveOneCurrentRow(t *testing.T) {
	// Extraction runs eight-wide, so two sources can assert the same key
	// at once.
	st, scope := testStore(t)
	var wg sync.WaitGroup
	for i, v := range []string{"cycles", "tram", "walks", "drives"} {
		wg.Add(1)
		go func(v string, i int) {
			defer wg.Done()
			f := &anamnesia.Fact{
				Scope: scope, FactKind: anamnesia.FactScopeUser, Key: "user.commute",
				Value: map[string]any{"v": v},
			}
			_ = st.UpsertFact(context.Background(), f)
		}(v, i)
	}
	wg.Wait()

	var current int
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM facts WHERE user_id = $1 AND key = 'user.commute'
		   AND deleted_at IS NULL AND superseded_by IS NULL`, scope.UserID).Scan(&current); err != nil {
		t.Fatalf("count: %v", err)
	}
	if current != 1 {
		t.Errorf("current rows = %d, want exactly 1 after concurrent writers", current)
	}
}
