package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// RecordCommitment inserts a new commitment. Owner/Beneficiary default
// to "user" when empty; status defaults to open.
func (s *Store) RecordCommitment(ctx context.Context, c *anamnesia.Commitment) error {
	if c.Body == "" {
		return errors.New("commitment: body required")
	}
	if c.Owner == "" {
		c.Owner = "user"
	}
	if c.Beneficiary == "" {
		c.Beneficiary = "user"
	}
	if !c.Status.Valid() {
		c.Status = anamnesia.CommitmentOpen
	}
	return s.Pool.QueryRow(ctx, `
		INSERT INTO commitments
			(user_id, project_id, owner, beneficiary, body, due_at, status, source_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at, updated_at`,
		c.Scope.UserID, c.Scope.ProjectID, c.Owner, c.Beneficiary,
		c.Body, c.DueAt, string(c.Status), c.SourceID,
	).Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt)
}

// ListCommitments returns commitments in scope. status="" matches any.
// Sort: open first, then earliest due, then newest.
func (s *Store) ListCommitments(
	ctx context.Context, scope anamnesia.Scope,
	status anamnesia.CommitmentStatus, limit int,
) ([]*anamnesia.Commitment, error) {
	if limit <= 0 {
		limit = 50
	}
	args := []any{scope.UserID}
	where := []string{"user_id = $1"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("project_id = $%d", len(args)))
	}
	if status != "" {
		args = append(args, string(status))
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT id, user_id, project_id, owner, beneficiary, body, due_at,
		       status, source_id, created_at, updated_at
		FROM commitments WHERE %s
		ORDER BY (status = 'open') DESC,
		         due_at ASC NULLS LAST,
		         created_at DESC
		LIMIT $%d`, strings.Join(where, " AND "), len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Commitment
	for rows.Next() {
		c, err := scanCommitment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResolveCommitment marks a commitment done or dropped. Re-opening is
// not allowed — record a new commitment instead.
func (s *Store) ResolveCommitment(ctx context.Context, id uuid.UUID, status anamnesia.CommitmentStatus) error {
	if !status.Valid() || status == anamnesia.CommitmentOpen {
		return fmt.Errorf("resolve status must be done or dropped, got %q", status)
	}
	tag, err := s.Pool.Exec(ctx,
		`UPDATE commitments SET status = $2, updated_at = now() WHERE id = $1`,
		id, string(status))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanCommitment(row rowScanner) (*anamnesia.Commitment, error) {
	var (
		c       anamnesia.Commitment
		project *uuid.UUID
		due     *time.Time
		status  string
		srcID   *uuid.UUID
	)
	err := row.Scan(&c.ID, &c.Scope.UserID, &project, &c.Owner, &c.Beneficiary,
		&c.Body, &due, &status, &srcID, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Scope.ProjectID = project
	c.DueAt = due
	c.Status = anamnesia.CommitmentStatus(status)
	c.SourceID = srcID
	return &c, nil
}
