package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// embeddingColumnsInSchema lists every table the database actually
// declares an `embedding` column on.
func embeddingColumnsInSchema(t *testing.T, st *Store) []string {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(), `
		SELECT c.relname
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE a.attname = 'embedding' AND a.attnum > 0 AND NOT a.attisdropped
		  AND c.relkind = 'r' AND n.nspname = 'public'
		ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("list embedding columns: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	return out
}

// TestEmbeddingTablesListsEveryEmbeddingColumn fails the moment a table
// carrying an embedding column is added to the schema without being added
// to embeddingTables.
//
// That omission is invisible at runtime, which is why it needs a test
// rather than care: `migrate --dims` would re-dimension only the tables it
// knows about, leaving the new column at the old width, while a health
// check that sampled one known table went on reporting green. Every
// embedding write into the new column would fail against a passing check.
func TestEmbeddingTablesListsEveryEmbeddingColumn(t *testing.T) {
	st, _ := testStore(t)

	found := embeddingColumnsInSchema(t, st)
	want := append([]string(nil), embeddingTables...)
	sort.Strings(found)
	sort.Strings(want)

	if strings.Join(found, ",") != strings.Join(want, ",") {
		t.Errorf("embeddingTables is %v, but the schema declares embedding columns on %v.\n"+
			"Add the missing table to embeddingTables, or `migrate --dims` will skip it.",
			want, found)
	}
}

// TestEmbeddingDimsRefusesColumnsThatDisagree covers the other half: even
// with every table registered, one column at the wrong width has to be
// reported rather than averaged away or sampled past. `/v1/health` must be
// able to fail, and this is one of the ways it must.
func TestEmbeddingDimsRefusesColumnsThatDisagree(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	original, err := st.EmbeddingDims(ctx)
	if err != nil {
		t.Fatalf("EmbeddingDims on a healthy schema: %v", err)
	}
	// Break the last registered table, whichever it is.
	victim := embeddingTables[len(embeddingTables)-1]
	t.Cleanup(func() {
		if err := st.SetEmbeddingDims(context.Background(), original); err != nil {
			t.Fatalf("restore %s to vector(%d): %v", victim, original, err)
		}
	})

	odd := original + 8
	if _, err := st.Pool.Exec(ctx, fmt.Sprintf(
		`DROP INDEX IF EXISTS %s_embedding`, victim)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, fmt.Sprintf(
		`ALTER TABLE %s ALTER COLUMN embedding TYPE vector(%d) USING NULL`, victim, odd)); err != nil {
		t.Fatal(err)
	}

	_, err = st.EmbeddingDims(ctx)
	if err == nil {
		t.Fatalf("EmbeddingDims reported success with %s at vector(%d) and the rest at vector(%d)",
			victim, odd, original)
	}
	if !strings.Contains(err.Error(), victim) {
		t.Errorf("the error does not name the table that disagrees: %v", err)
	}

	// migrate --dims is the documented repair, so it has to work from
	// exactly the state the check refuses.
	if err := st.SetEmbeddingDims(ctx, original); err != nil {
		t.Fatalf("SetEmbeddingDims could not repair a disagreement: %v", err)
	}
	if got, err := st.EmbeddingDims(ctx); err != nil || got != original {
		t.Fatalf("after repair: dims=%d err=%v, want %d and no error", got, err, original)
	}
}

// TestEveryEmbeddingTableHasAnANNIndex guards the other thing that only
// shows up as slowness: a table registered for re-dimensioning but never
// given an index falls back to sequential scan on every search.
func TestEveryEmbeddingTableHasAnANNIndex(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	dims, err := st.EmbeddingDims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ANNIndexableDims(dims) {
		t.Skipf("vector(%d) is above the HNSW ceiling, so no index is expected", dims)
	}
	missing, err := st.MissingANNIndexes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("no ANN index on %v, so vector search there is a sequential scan", missing)
	}
}
