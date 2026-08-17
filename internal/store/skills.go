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

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// RegisterSkill upserts a skill keyed by (scope.user_id, scope.project_id, name).
func (s *Store) RegisterSkill(ctx context.Context, sk *anamnesia.Skill) error {
	if sk.Name == "" {
		return errors.New("skill: name required")
	}
	if !sk.Kind.Valid() {
		sk.Kind = anamnesia.SkillFunction
	}
	var sigJSON, metaJSON []byte
	if sk.Signature != nil {
		b, err := json.Marshal(sk.Signature)
		if err != nil {
			return fmt.Errorf("marshal signature: %w", err)
		}
		sigJSON = b
	}
	if sk.Meta != nil {
		b, err := json.Marshal(sk.Meta)
		if err != nil {
			return fmt.Errorf("marshal meta: %w", err)
		}
		metaJSON = b
	}
	return s.Tx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO skills (user_id, project_id, name, kind, description, signature, body, meta)
			VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8::jsonb)
			ON CONFLICT (user_id, coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid), name)
				WHERE deleted_at IS NULL
			DO UPDATE SET
				kind        = EXCLUDED.kind,
				description = EXCLUDED.description,
				signature   = EXCLUDED.signature,
				body        = EXCLUDED.body,
				meta        = EXCLUDED.meta
			RETURNING id`,
			sk.Scope.UserID, sk.Scope.ProjectID, sk.Name, string(sk.Kind),
			nilOrString(sk.Description), nilOrJSON(sigJSON),
			nilOrString(sk.Body), nilOrJSON(metaJSON),
		).Scan(&sk.ID)
	})
}

// ListSkills returns active skills in scope.
func (s *Store) ListSkills(ctx context.Context, scope anamnesia.Scope, limit int) ([]*anamnesia.Skill, error) {
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
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT id, user_id, project_id, name, kind, description, signature, body, meta,
		       use_count, last_used_at
		FROM skills WHERE %s ORDER BY name ASC LIMIT $%d`,
		strings.Join(where, " AND "), len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Skill
	for rows.Next() {
		sk, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// ForgetSkill soft-deletes a skill.
func (s *Store) ForgetSkill(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE skills SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// BumpSkillUse increments use_count and stamps last_used_at.
func (s *Store) BumpSkillUse(ctx context.Context, id uuid.UUID) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE skills SET use_count = use_count + 1, last_used_at = now() WHERE id = $1`, id)
	return err
}

func scanSkill(row rowScanner) (*anamnesia.Skill, error) {
	var (
		sk         anamnesia.Skill
		project    *uuid.UUID
		desc, body *string
		sigJSON    []byte
		metaJSON   []byte
		lastUsed   *time.Time
	)
	err := row.Scan(
		&sk.ID, &sk.Scope.UserID, &project, &sk.Name, &sk.Kind,
		&desc, &sigJSON, &body, &metaJSON,
		&sk.UseCount, &lastUsed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sk.Scope.ProjectID = project
	if desc != nil {
		sk.Description = *desc
	}
	if body != nil {
		sk.Body = *body
	}
	if len(sigJSON) > 0 {
		_ = json.Unmarshal(sigJSON, &sk.Signature)
	}
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &sk.Meta)
	}
	sk.LastUsedAt = lastUsed
	return &sk, nil
}
