package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

// UpsertFact creates a new fact or updates an existing one keyed by
// (scope, fact_scope, key). Embeddings are written when non-nil.
func (s *Store) UpsertFact(ctx context.Context, f *anamnesia.Fact) error {
	if f.Key == "" {
		return errors.New("fact: key required")
	}
	if !f.FactKind.Valid() {
		f.FactKind = anamnesia.FactScopeProject
	}
	if f.Value == nil {
		f.Value = map[string]any{}
	}
	if f.IngestedAt.IsZero() {
		f.IngestedAt = time.Now().UTC()
	}
	if f.ValidFrom.IsZero() {
		f.ValidFrom = f.IngestedAt
	}
	if f.Trust == 0 {
		f.Trust = 0.5
	}

	valueJSON, err := json.Marshal(f.Value)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}

	var emb *pgvector.Vector
	if len(f.Embedding) > 0 {
		v := pgvector.NewVector(f.Embedding)
		emb = &v
	}

	piiTags := f.PIITags
	if piiTags == nil {
		piiTags = []string{}
	}

	return s.Tx(ctx, func(tx pgx.Tx) error {
		// Identity = (user_id, project_id-or-zero, fact_scope, key).
		// On conflict, merge: bump valid_from + ingested_at, replace value, etc.
		var id uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO facts
				(user_id, project_id, source_id, fact_scope, key, value, source, trust, pii_tags,
				 embedding, embed_model, valid_from, ingested_at)
			VALUES
				($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (user_id, coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid), fact_scope, key)
				WHERE deleted_at IS NULL
			DO UPDATE SET
				value       = EXCLUDED.value,
				source      = COALESCE(EXCLUDED.source, facts.source),
				source_id   = COALESCE(EXCLUDED.source_id, facts.source_id),
				trust       = EXCLUDED.trust,
				pii_tags    = EXCLUDED.pii_tags,
				embedding   = COALESCE(EXCLUDED.embedding, facts.embedding),
				embed_model = COALESCE(EXCLUDED.embed_model, facts.embed_model),
				ingested_at = EXCLUDED.ingested_at
			RETURNING id`,
			f.Scope.UserID, f.Scope.ProjectID, f.SourceID, string(f.FactKind), f.Key, string(valueJSON),
			f.Source, f.Trust, piiTags, emb, f.EmbedModel, f.ValidFrom, f.IngestedAt,
		).Scan(&id)
		if err != nil {
			return err
		}
		f.ID = id
		return nil
	})
}

// GetFact returns one fact by ID, or ErrNotFound.
func (s *Store) GetFact(ctx context.Context, id uuid.UUID) (*anamnesia.Fact, error) {
	row := s.Pool.QueryRow(ctx, `
		SELECT id, user_id, project_id, source_id, fact_scope, key, value, source, trust, pii_tags,
		       embed_model, valid_from, valid_to, ingested_at, invalidated_at,
		       superseded_by, deleted_at
		FROM facts WHERE id = $1`, id)
	return scanFact(row)
}

// ListFacts returns facts in scope, filtered to non-deleted, ordered by
// ingested_at desc.
func (s *Store) ListFacts(ctx context.Context, scope anamnesia.Scope, factKind anamnesia.FactScope, limit int) ([]*anamnesia.Fact, error) {
	if limit <= 0 {
		limit = 50
	}
	var args []any
	args = append(args, scope.UserID)
	where := []string{"user_id = $1", "deleted_at IS NULL"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("project_id = $%d", len(args)))
	}
	if factKind != "" {
		args = append(args, string(factKind))
		where = append(where, fmt.Sprintf("fact_scope = $%d", len(args)))
	}
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT id, user_id, project_id, source_id, fact_scope, key, value, source, trust, pii_tags,
		       embed_model, valid_from, valid_to, ingested_at, invalidated_at,
		       superseded_by, deleted_at
		FROM facts WHERE %s
		ORDER BY ingested_at DESC
		LIMIT $%d`, strings.Join(where, " AND "), len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ForgetFact soft-deletes a fact.
func (s *Store) ForgetFact(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE facts SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FactsMissingEmbedding returns up to n facts with no embedding yet.
func (s *Store) FactsMissingEmbedding(ctx context.Context, n int) ([]*anamnesia.Fact, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, user_id, project_id, source_id, fact_scope, key, value, source, trust, pii_tags,
		       embed_model, valid_from, valid_to, ingested_at, invalidated_at,
		       superseded_by, deleted_at
		FROM facts
		WHERE deleted_at IS NULL AND embedding IS NULL
		ORDER BY ingested_at ASC
		LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetFactEmbedding updates the embedding columns on a fact.
func (s *Store) SetFactEmbedding(ctx context.Context, id uuid.UUID, vec []float32, model string) error {
	v := pgvector.NewVector(vec)
	_, err := s.Pool.Exec(ctx,
		`UPDATE facts SET embedding = $2, embed_model = $3 WHERE id = $1`,
		id, v, model)
	return err
}

// rowScanner is the common interface satisfied by *pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanFact(row rowScanner) (*anamnesia.Fact, error) {
	var (
		f        anamnesia.Fact
		project  *uuid.UUID
		sourceID *uuid.UUID
		factSc   string
		value    []byte
		source   *string
		piiTags  []string
		embMod   *string
		validTo  *time.Time
		invalid  *time.Time
		superBy  *uuid.UUID
		deleted  *time.Time
	)
	err := row.Scan(
		&f.ID, &f.Scope.UserID, &project, &sourceID, &factSc, &f.Key, &value, &source, &f.Trust, &piiTags,
		&embMod, &f.ValidFrom, &validTo, &f.IngestedAt, &invalid, &superBy, &deleted,
	)
	f.SourceID = sourceID
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.Scope.ProjectID = project
	f.FactKind = anamnesia.FactScope(factSc)
	if source != nil {
		f.Source = *source
	}
	if embMod != nil {
		f.EmbedModel = *embMod
	}
	f.PIITags = piiTags
	f.ValidTo = validTo
	f.InvalidatedAt = invalid
	f.SupersededBy = superBy
	f.DeletedAt = deleted
	if len(value) > 0 {
		_ = json.Unmarshal(value, &f.Value)
	}
	return &f, nil
}
