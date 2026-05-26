package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

// InsertSource writes a new sources row. ExtractionState defaults to
// 'pending'; the worker picks it up. If ExpiresAt is zero, default 7 days.
func (s *Store) InsertSource(ctx context.Context, src *anamnesia.Source) error {
	if src.Kind == "" {
		return errors.New("source: kind required")
	}
	if src.OccurredAt.IsZero() {
		src.OccurredAt = time.Now().UTC()
	}
	if src.IngestedAt.IsZero() {
		src.IngestedAt = time.Now().UTC()
	}
	if src.ExpiresAt.IsZero() {
		src.ExpiresAt = src.IngestedAt.Add(7 * 24 * time.Hour)
	}
	if src.ExtractionState == "" {
		src.ExtractionState = anamnesia.ExtractionPending
	}
	if src.Participants == nil {
		src.Participants = []string{}
	}
	var metaJSON []byte
	if src.Metadata != nil {
		b, err := json.Marshal(src.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
		metaJSON = b
	}
	return s.Pool.QueryRow(ctx, `
		INSERT INTO sources
			(user_id, project_id, kind, external_ref, title, participants,
			 occurred_at, ingested_at, raw_content, metadata, extraction_state,
			 preserve_raw, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12,$13)
		RETURNING id`,
		src.Scope.UserID, src.Scope.ProjectID, src.Kind,
		nilOrString(src.ExternalRef), nilOrString(src.Title), src.Participants,
		src.OccurredAt, src.IngestedAt, nilOrString(src.RawContent),
		nilOrJSON(metaJSON), src.ExtractionState, src.PreserveRaw, src.ExpiresAt,
	).Scan(&src.ID)
}

// GetSource returns a source by id.
func (s *Store) GetSource(ctx context.Context, id uuid.UUID) (*anamnesia.Source, error) {
	row := s.Pool.QueryRow(ctx, sourceSelect+` FROM sources WHERE id = $1`, id)
	return scanSource(row)
}

// ListPendingSources returns up to n sources waiting to be extracted,
// oldest first.
func (s *Store) ListPendingSources(ctx context.Context, n int) ([]*anamnesia.Source, error) {
	if n <= 0 {
		n = 32
	}
	rows, err := s.Pool.Query(ctx,
		sourceSelect+` FROM sources WHERE extraction_state = 'pending' ORDER BY ingested_at ASC LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// MarkExtracted stamps the row as done with an ops count.
func (s *Store) MarkExtracted(ctx context.Context, id uuid.UUID, opsProduced int) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE sources
		   SET extraction_state = 'done',
		       extracted_at     = now(),
		       ops_produced     = $2,
		       extraction_error = NULL
		 WHERE id = $1`, id, opsProduced)
	return err
}

// MarkSkipped is called when the gate decides nothing is worth extracting.
func (s *Store) MarkSkipped(ctx context.Context, id uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE sources
		   SET extraction_state = 'skipped',
		       extracted_at     = now(),
		       ops_produced     = 0
		 WHERE id = $1`, id)
	return err
}

// MarkFailed records an extraction error. The worker may retry by
// resetting the row to 'pending' explicitly.
func (s *Store) MarkFailed(ctx context.Context, id uuid.UUID, errMsg string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE sources
		   SET extraction_state = 'failed',
		       extracted_at     = now(),
		       extraction_error = $2
		 WHERE id = $1`, id, errMsg)
	return err
}

// PurgeExpiredSourceContent nulls out raw_content on sources past their
// TTL (preserve_raw=true rows are exempt). Returns the count nulled.
func (s *Store) PurgeExpiredSourceContent(ctx context.Context) (int, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE sources
		   SET raw_content = NULL
		 WHERE raw_content IS NOT NULL
		   AND preserve_raw = FALSE
		   AND expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ListSourcesInWindow returns sources by occurred_at range, filtered by
// kind when non-empty. Used by the temporal-aggregation paths.
func (s *Store) ListSourcesInWindow(ctx context.Context, scope anamnesia.Scope, start, end time.Time, kind string, limit int) ([]*anamnesia.Source, error) {
	if limit <= 0 {
		limit = 100
	}
	args := []any{scope.UserID, start, end}
	where := `user_id = $1 AND occurred_at >= $2 AND occurred_at < $3`
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where += fmt.Sprintf(" AND project_id = $%d", len(args))
	}
	if kind != "" {
		args = append(args, kind)
		where += fmt.Sprintf(" AND kind = $%d", len(args))
	}
	args = append(args, limit)
	q := sourceSelect + ` FROM sources WHERE ` + where +
		fmt.Sprintf(` ORDER BY occurred_at DESC LIMIT $%d`, len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

const sourceSelect = `SELECT id, user_id, project_id, kind, external_ref, title,
		participants, occurred_at, ingested_at, raw_content, metadata,
		extraction_state, extracted_at, extraction_error, ops_produced,
		preserve_raw, expires_at`

func scanSource(row rowScanner) (*anamnesia.Source, error) {
	var (
		src         anamnesia.Source
		project     *uuid.UUID
		extRef      *string
		title       *string
		raw         *string
		metaJSON    []byte
		extractedAt *time.Time
		errMsg      *string
	)
	err := row.Scan(
		&src.ID, &src.Scope.UserID, &project, &src.Kind, &extRef, &title,
		&src.Participants, &src.OccurredAt, &src.IngestedAt, &raw, &metaJSON,
		&src.ExtractionState, &extractedAt, &errMsg, &src.OpsProduced,
		&src.PreserveRaw, &src.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	src.Scope.ProjectID = project
	if extRef != nil {
		src.ExternalRef = *extRef
	}
	if title != nil {
		src.Title = *title
	}
	if raw != nil {
		src.RawContent = *raw
	}
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &src.Metadata)
	}
	src.ExtractedAt = extractedAt
	if errMsg != nil {
		src.ExtractionError = *errMsg
	}
	return &src, nil
}
