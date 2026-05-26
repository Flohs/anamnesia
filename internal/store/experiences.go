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

// RecordExperience appends a new experience row. Use SupersedeExperience
// to mark a previous experience as superseded by this one.
func (s *Store) RecordExperience(ctx context.Context, e *anamnesia.Experience) error {
	if e.Body == "" {
		return errors.New("experience: body required")
	}
	if !e.Kind.Valid() {
		e.Kind = anamnesia.ExperienceCase
	}
	if e.IngestedAt.IsZero() {
		e.IngestedAt = time.Now().UTC()
	}
	if e.ValidFrom.IsZero() {
		e.ValidFrom = e.IngestedAt
	}
	if e.LastUsedAt.IsZero() {
		e.LastUsedAt = e.IngestedAt
	}
	if e.Trust == 0 {
		e.Trust = 0.5
	}
	if e.Importance == 0 {
		e.Importance = 0.5
	}
	var metaJSON []byte
	if e.Meta != nil {
		b, err := json.Marshal(e.Meta)
		if err != nil {
			return fmt.Errorf("marshal meta: %w", err)
		}
		metaJSON = b
	}
	var emb *pgvector.Vector
	if len(e.Embedding) > 0 {
		v := pgvector.NewVector(e.Embedding)
		emb = &v
	}
	piiTags := e.PIITags
	if piiTags == nil {
		piiTags = []string{}
	}
	participants := e.Participants
	if participants == nil {
		participants = []string{}
	}
	rel := e.Relevance
	if rel == 0 {
		rel = e.Importance
	}
	occurredAt := e.OccurredAt
	if occurredAt == nil {
		t := e.IngestedAt
		occurredAt = &t
	}
	var provJSON []byte
	if e.Provenance != nil {
		b, err := json.Marshal(e.Provenance)
		if err != nil {
			return fmt.Errorf("marshal provenance: %w", err)
		}
		provJSON = b
	}
	return s.Pool.QueryRow(ctx, `
		INSERT INTO experiences
			(user_id, project_id, source_id, kind, abstraction, title, body, outcome, meta,
			 trust, importance, relevance, pii_tags,
			 embedding, embed_model, valid_from, ingested_at, last_used_at,
			 occurred_at, participants, topic, parent_id, provenance)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23::jsonb)
		RETURNING id`,
		e.Scope.UserID, e.Scope.ProjectID, e.SourceID, string(e.Kind), e.Abstraction,
		nilOrString(e.Title), e.Body, nilOrString(string(e.Outcome)),
		nilOrJSON(metaJSON), e.Trust, e.Importance, rel, piiTags,
		emb, nilOrString(e.EmbedModel), e.ValidFrom, e.IngestedAt, e.LastUsedAt,
		occurredAt, participants, nilOrString(e.Topic), e.ParentID, nilOrJSON(provJSON),
	).Scan(&e.ID)
}

// SupersedeExperience marks oldID as superseded by newID and stamps
// invalidated_at. Both rows must already exist.
func (s *Store) SupersedeExperience(ctx context.Context, oldID, newID uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE experiences
		   SET superseded_by = $2, invalidated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL`,
		oldID, newID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ForgetExperience soft-deletes an experience.
func (s *Store) ForgetExperience(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE experiences SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetExperience returns one experience by ID.
func (s *Store) GetExperience(ctx context.Context, id uuid.UUID) (*anamnesia.Experience, error) {
	row := s.Pool.QueryRow(ctx, expSelectCols+` FROM experiences WHERE id = $1`, id)
	return scanExperience(row)
}

// ListExperiences returns recent experiences in scope.
func (s *Store) ListExperiences(ctx context.Context, scope anamnesia.Scope, limit int) ([]*anamnesia.Experience, error) {
	if limit <= 0 {
		limit = 20
	}
	var args []any
	args = append(args, scope.UserID)
	where := []string{"user_id = $1", "deleted_at IS NULL", "invalidated_at IS NULL"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("project_id = $%d", len(args)))
	}
	args = append(args, limit)
	q := fmt.Sprintf(expSelectCols+` FROM experiences WHERE %s ORDER BY ingested_at DESC LIMIT $%d`,
		strings.Join(where, " AND "), len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Experience
	for rows.Next() {
		e, err := scanExperience(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExperiencesMissingEmbedding returns up to n experiences without embedding.
func (s *Store) ExperiencesMissingEmbedding(ctx context.Context, n int) ([]*anamnesia.Experience, error) {
	rows, err := s.Pool.Query(ctx,
		expSelectCols+` FROM experiences WHERE deleted_at IS NULL AND embedding IS NULL ORDER BY ingested_at ASC LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Experience
	for rows.Next() {
		e, err := scanExperience(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetExperienceEmbedding updates the embedding columns on an experience.
func (s *Store) SetExperienceEmbedding(ctx context.Context, id uuid.UUID, vec []float32, model string) error {
	v := pgvector.NewVector(vec)
	_, err := s.Pool.Exec(ctx,
		`UPDATE experiences SET embedding = $2, embed_model = $3 WHERE id = $1`,
		id, v, model)
	return err
}

const expSelectCols = `SELECT id, user_id, project_id, source_id, kind, abstraction, title, body, outcome, meta,
		trust, importance, relevance, pii_tags, use_count, last_used_at, embed_model,
		valid_from, valid_to, ingested_at, invalidated_at, superseded_by, deleted_at,
		occurred_at, participants, topic, parent_id, provenance`

func scanExperience(row rowScanner) (*anamnesia.Experience, error) {
	var (
		e            anamnesia.Experience
		project      *uuid.UUID
		sourceID     *uuid.UUID
		title        *string
		outcome      *string
		metaJSON     []byte
		piiTags      []string
		embMod       *string
		validTo      *time.Time
		invalid      *time.Time
		superBy      *uuid.UUID
		deleted      *time.Time
		occurredAt   *time.Time
		participants []string
		topic        *string
		parentID     *uuid.UUID
		provJSON     []byte
	)
	err := row.Scan(
		&e.ID, &e.Scope.UserID, &project, &sourceID, &e.Kind, &e.Abstraction,
		&title, &e.Body, &outcome, &metaJSON,
		&e.Trust, &e.Importance, &e.Relevance, &piiTags, &e.UseCount, &e.LastUsedAt, &embMod,
		&e.ValidFrom, &validTo, &e.IngestedAt, &invalid, &superBy, &deleted,
		&occurredAt, &participants, &topic, &parentID, &provJSON,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.Scope.ProjectID = project
	e.SourceID = sourceID
	if title != nil {
		e.Title = *title
	}
	if outcome != nil {
		e.Outcome = anamnesia.Outcome(*outcome)
	}
	if embMod != nil {
		e.EmbedModel = *embMod
	}
	e.PIITags = piiTags
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &e.Meta)
	}
	e.ValidTo = validTo
	e.InvalidatedAt = invalid
	e.SupersededBy = superBy
	e.DeletedAt = deleted
	e.OccurredAt = occurredAt
	e.Participants = participants
	if topic != nil {
		e.Topic = *topic
	}
	e.ParentID = parentID
	if len(provJSON) > 0 {
		_ = json.Unmarshal(provJSON, &e.Provenance)
	}
	return &e, nil
}

func nilOrString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilOrJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}
