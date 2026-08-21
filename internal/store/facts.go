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

	"github.com/flohs/anamnesia/pkg/anamnesia"
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
		// Identity = (user_id, project_id-or-zero, fact_scope, key), and
		// only among *current* rows: superseded ones are exempt from
		// facts_identity (see migration 0010), which is what lets a key
		// keep its previous values.
		//
		// This is not an upsert. A changed value has to become a second
		// row while the old one survives, and ON CONFLICT DO UPDATE can
		// only ever mutate the row it collided with. FOR UPDATE serialises
		// concurrent writers of one key, which matters because extraction
		// runs several sources at once; the partial unique index is the
		// backstop if one still loses the race.
		var (
			curID    uuid.UUID
			sameVal  bool
			curFound = true
		)
		err := tx.QueryRow(ctx, `
			SELECT id, value = $5::jsonb
			  FROM facts
			 WHERE user_id = $1
			   AND coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid)
			     = coalesce($2::uuid, '00000000-0000-0000-0000-000000000000'::uuid)
			   AND fact_scope = $3 AND key = $4
			   AND deleted_at IS NULL AND superseded_by IS NULL
			   FOR UPDATE`,
			f.Scope.UserID, f.Scope.ProjectID, string(f.FactKind), f.Key, string(valueJSON),
		).Scan(&curID, &sameVal)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			curFound = false
		case err != nil:
			return err
		}

		// Re-asserting the value already stored is not a change, and the
		// extractor re-asserts constantly. Updating in place keeps a
		// version per mention out of the history, and keeps provenance and
		// the embedding with the source that actually authored the value.
		if curFound && sameVal {
			if _, err := tx.Exec(ctx, `
				UPDATE facts SET
					source      = COALESCE($2, source),
					trust       = $3,
					pii_tags    = $4,
					embedding   = COALESCE($5, embedding),
					embed_model = COALESCE($6, embed_model),
					ingested_at = $7
				WHERE id = $1`,
				curID, f.Source, f.Trust, piiTags, emb, f.EmbedModel, f.IngestedAt,
			); err != nil {
				return err
			}
			f.ID = curID
			return nil
		}

		// The new row's id is generated here so the old row can point at
		// it before it exists. superseded_by carries no foreign key, and
		// the order matters: stamping the old row first takes it out of
		// facts_identity, which is what makes room for the insert.
		newID := uuid.New()
		if curFound {
			if _, err := tx.Exec(ctx, `
				UPDATE facts
				   SET superseded_by = $2, valid_to = now(), invalidated_at = now()
				 WHERE id = $1`, curID, newID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO facts
				(id, user_id, project_id, source_id, fact_scope, key, value, source, trust, pii_tags,
				 embedding, embed_model, valid_from, ingested_at)
			VALUES
				($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13, $14)`,
			newID, f.Scope.UserID, f.Scope.ProjectID, f.SourceID, string(f.FactKind), f.Key,
			string(valueJSON), f.Source, f.Trust, piiTags, emb, f.EmbedModel, f.ValidFrom, f.IngestedAt,
		); err != nil {
			return err
		}
		f.ID = newID
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
	where := []string{"user_id = $1", "deleted_at IS NULL", "superseded_by IS NULL"}
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
