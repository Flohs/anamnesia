package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// ListCapabilities returns active skills in scope, sorted by freshness
// (last_used_at DESC NULLS LAST, then use_count DESC). Boot-shaped view
// — agents call this to discover what tools/skills they can lean on.
// Counterpart to ListSkills (name-sorted, write-shaped).
func (s *Store) ListCapabilities(ctx context.Context, scope anamnesia.Scope, limit int) ([]*anamnesia.Skill, error) {
	if limit <= 0 {
		limit = 50
	}
	args := []any{scope.UserID}
	where := []string{"user_id = $1", "deleted_at IS NULL"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("(project_id = $%d OR project_id IS NULL)", len(args)))
	}
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT id, user_id, project_id, name, kind, description, signature, body, meta,
		       use_count, last_used_at
		FROM skills WHERE %s
		ORDER BY last_used_at DESC NULLS LAST, use_count DESC, name ASC
		LIMIT $%d`, strings.Join(where, " AND "), len(args))
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
