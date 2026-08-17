package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// GetIdentity returns the user's Identity by scanning facts under the
// reserved key prefixes user.persona.* and user.profile.*. The render
// is filled in by RenderIdentity.
//
// Scope.ProjectID is ignored deliberately: identity is a user-level
// concept and follows the user across projects.
func (s *Store) GetIdentity(ctx context.Context, scope anamnesia.Scope) (anamnesia.Identity, error) {
	out := anamnesia.Identity{
		Scope:   anamnesia.Scope{UserID: scope.UserID},
		Persona: map[string]any{},
		Profile: map[string]any{},
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT key, value
		FROM facts
		WHERE user_id = $1
		  AND deleted_at IS NULL
		  AND (key LIKE 'user.persona.%' OR key LIKE 'user.profile.%')
		ORDER BY ingested_at DESC`, scope.UserID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	// Newest-first wins per key (subsequent iterations skip seen keys).
	seen := map[string]bool{}
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return out, err
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		val := unpackFactValue(raw)
		switch {
		case strings.HasPrefix(key, "user.persona."):
			out.Persona[strings.TrimPrefix(key, "user.persona.")] = val
		case strings.HasPrefix(key, "user.profile."):
			out.Profile[strings.TrimPrefix(key, "user.profile.")] = val
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.SystemPrompt = RenderIdentity(out)
	return out, nil
}

// unpackFactValue returns the most useful inner type from a fact value
// jsonb blob. Facts are stored as JSON objects; persona/profile values
// are usually a single primitive — return the "v" key if it exists,
// else the whole object.
func unpackFactValue(raw []byte) any {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return string(raw)
	}
	if v, ok := obj["v"]; ok {
		return v
	}
	return obj
}
