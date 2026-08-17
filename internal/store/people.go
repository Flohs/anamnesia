package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// ListPeople returns entities of kind="person" within scope, decorated
// with recent-mention counts (last 90d) and outgoing edges. People are
// scoped by user only, not project — a person is the same person across
// the user's projects.
func (s *Store) ListPeople(ctx context.Context, scope anamnesia.Scope, limit int) ([]anamnesia.PersonView, error) {
	if limit <= 0 {
		limit = 50
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -90)
	rows, err := s.Pool.Query(ctx, `
		SELECT e.id, e.user_id, e.project_id, e.kind, e.name, e.props, e.created_at,
		       (SELECT count(*) FROM experiences x
		         WHERE x.user_id = e.user_id
		           AND x.deleted_at IS NULL
		           AND x.occurred_at >= $2
		           AND e.name = ANY(x.participants)) AS recent_mentions,
		       (SELECT max(x.occurred_at) FROM experiences x
		         WHERE x.user_id = e.user_id
		           AND x.deleted_at IS NULL
		           AND e.name = ANY(x.participants)) AS last_mentioned_at
		FROM entities e
		WHERE e.user_id = $1
		  AND e.kind = 'person'
		ORDER BY recent_mentions DESC NULLS LAST, e.name ASC
		LIMIT $3`, scope.UserID, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []anamnesia.PersonView
	for rows.Next() {
		var (
			ent      anamnesia.Entity
			props    []byte
			project  *uuid.UUID
			mentions int
			last     *time.Time
		)
		if err := rows.Scan(&ent.ID, &ent.Scope.UserID, &project, &ent.Kind,
			&ent.Name, &props, &ent.CreatedAt, &mentions, &last); err != nil {
			return nil, err
		}
		ent.Scope.ProjectID = project
		if len(props) > 0 {
			_ = json.Unmarshal(props, &ent.Props)
		}
		view := anamnesia.PersonView{
			Entity: &ent, RecentMentions: mentions, LastMentionedAt: last,
		}
		// Fetch outbound edges (cap at 5 per person). Failures here are
		// non-fatal — we'd rather return the person without edges than
		// fail the whole list.
		neighbors, edges, _ := s.Neighbors(ctx, ent.ID, nil, "out", 5)
		nameByID := map[uuid.UUID]string{}
		for _, n := range neighbors {
			nameByID[n.ID] = n.Name
		}
		for _, e := range edges {
			view.Edges = append(view.Edges, anamnesia.PersonEdge{
				Kind: e.Kind, ToName: nameByID[e.To], ToID: e.To.String(),
			})
		}
		out = append(out, view)
	}
	return out, rows.Err()
}
