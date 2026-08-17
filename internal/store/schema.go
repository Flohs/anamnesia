// schema.go introspects and re-dimensions the embedding columns.
//
// The embedding width is a deployment decision (it follows whichever embed
// model you configure), but the schema has to agree with it or every
// embedding write fails at insert time with a pgvector dimension error.
// The server checks these on boot rather than discovering the mismatch one
// failed write at a time.
package store

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// hnswMaxDims is pgvector's ceiling for an HNSW index on the `vector`
// type. Above it the column still works, but only via sequential scan.
const hnswMaxDims = 2000

// embeddingTables are the three tables carrying an embedding column.
// Their widths are always changed together.
var embeddingTables = []string{"facts", "experiences", "entities"}

var vectorTypeRE = regexp.MustCompile(`^(?:vector|halfvec)\((\d+)\)$`)

// EmbeddingDims reports the vector width the schema is currently built
// for, read from facts.embedding. All three embedding columns are kept in
// lockstep, so one is representative.
func (s *Store) EmbeddingDims(ctx context.Context) (int, error) {
	var declared string
	err := s.Pool.QueryRow(ctx, `
		SELECT format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		WHERE c.relname = 'facts' AND a.attname = 'embedding' AND a.attnum > 0
	`).Scan(&declared)
	if err != nil {
		return 0, fmt.Errorf("read facts.embedding type: %w", err)
	}
	m := vectorTypeRE.FindStringSubmatch(declared)
	if m == nil {
		return 0, fmt.Errorf("facts.embedding has unexpected type %q", declared)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("parse dimension from %q: %w", declared, err)
	}
	return n, nil
}

// MissingANNIndexes returns the embedding tables that have no ANN index.
// A fresh install should have none missing; a non-empty result means
// vector search is falling back to sequential scans.
func (s *Store) MissingANNIndexes(ctx context.Context) ([]string, error) {
	var missing []string
	for _, t := range embeddingTables {
		var exists bool
		err := s.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`,
			t+"_embedding",
		).Scan(&exists)
		if err != nil {
			return nil, err
		}
		if !exists {
			missing = append(missing, t)
		}
	}
	return missing, nil
}

// SetEmbeddingDims re-dimensions all three embedding columns and rebuilds
// their ANN indexes.
//
// This discards existing vectors: pgvector cannot reinterpret a stored
// vector at a different width, and a vector produced by a different model
// is not comparable anyway. Setting the column to NULL is what makes the
// embed backfill worker pick the rows up again on its next tick, so
// retrieval heals itself without further intervention.
func (s *Store) SetEmbeddingDims(ctx context.Context, dims int) error {
	if dims <= 0 {
		return fmt.Errorf("embedding dimensions must be positive, got %d", dims)
	}
	current, err := s.EmbeddingDims(ctx)
	if err != nil {
		return err
	}
	missing, err := s.MissingANNIndexes(ctx)
	if err != nil {
		return err
	}
	if current == dims && len(missing) == 0 {
		return nil // already correct, nothing to rebuild
	}

	return s.Tx(ctx, func(tx pgx.Tx) error {
		for _, t := range embeddingTables {
			if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s_embedding`, t)); err != nil {
				return fmt.Errorf("drop %s_embedding: %w", t, err)
			}
		}
		for _, t := range embeddingTables {
			// USING NULL is required: there is no cast between vector
			// widths, so the old value cannot be carried over.
			stmt := fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN embedding TYPE vector(%d) USING NULL`, t, dims)
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("alter %s.embedding to vector(%d): %w", t, dims, err)
			}
		}
		if dims > hnswMaxDims {
			// Deliberately no index: pgvector rejects HNSW above 2000
			// dimensions. Caller warns; retrieval still works by scan.
			return nil
		}
		for _, t := range embeddingTables {
			stmt := fmt.Sprintf(
				`CREATE INDEX %s_embedding ON %s USING hnsw(embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64)`,
				t, t)
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("create %s_embedding: %w", t, err)
			}
		}
		return nil
	})
}

// ANNIndexableDims reports whether dims can carry an HNSW index.
func ANNIndexableDims(dims int) bool { return dims <= hnswMaxDims }

// MigrationVersion returns the goose schema version currently applied.
func (s *Store) MigrationVersion(ctx context.Context) (int64, error) {
	if err := goose.SetDialect("postgres"); err != nil {
		return 0, err
	}
	db := stdlib.OpenDBFromPool(s.Pool)
	defer db.Close()
	return goose.GetDBVersionContext(ctx, db)
}
