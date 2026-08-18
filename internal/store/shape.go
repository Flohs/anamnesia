// shape.go answers two questions about the shape of a memory store:
// when things were written, and where they sit relative to each other.
//
// Both are aggregate reads for the UI. Neither is on any hot path.
package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// ActivityBucket is one day of one project.
type ActivityBucket struct {
	Date        string  // YYYY-MM-DD
	Project     *string // nil for user-level rows, which belong to no project
	Sources     int
	Facts       int
	Experiences int
}

// ActivityBuckets counts what was written per day per project over the
// last n days. Days with nothing in them are absent rather than zero:
// the caller knows the range it asked for, and an empty day is the
// absence of a bucket.
func (s *Store) ActivityBuckets(ctx context.Context, scope anamnesia.Scope, days int) ([]ActivityBucket, error) {
	if days <= 0 {
		days = 90
	}
	if days > 366 {
		days = 366
	}
	since := time.Now().UTC().AddDate(0, 0, -days)

	args := []any{scope.UserID, since}
	projectFilter := ""
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		projectFilter = " AND (t.project_id = $3 OR t.project_id IS NULL)"
	}
	part := func(table, kind, extra string) string {
		return fmt.Sprintf(`
			SELECT date_trunc('day', t.ingested_at)::date AS day, p.slug AS slug,
			       '%s' AS kind, count(*) AS n
			  FROM %s t LEFT JOIN projects p ON p.id = t.project_id
			 WHERE t.user_id = $1 AND t.ingested_at >= $2%s%s
			 GROUP BY 1, 2`, kind, table, extra, projectFilter)
	}
	q := strings.Join([]string{
		part("sources", "sources", ""),
		part("facts", "facts", " AND t.deleted_at IS NULL"),
		part("experiences", "experiences", " AND t.deleted_at IS NULL"),
	}, "\nUNION ALL\n") + "\nORDER BY 1 ASC, 2 ASC NULLS FIRST"

	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type key struct {
		day  string
		slug string
	}
	index := map[key]int{}
	out := []ActivityBucket{}
	for rows.Next() {
		var (
			day  time.Time
			slug *string
			kind string
			n    int
		)
		if err := rows.Scan(&day, &slug, &kind, &n); err != nil {
			return nil, err
		}
		k := key{day: day.Format("2006-01-02")}
		if slug != nil {
			k.slug = *slug
		}
		i, seen := index[k]
		if !seen {
			out = append(out, ActivityBucket{Date: k.day, Project: slug})
			i = len(out) - 1
			index[k] = i
		}
		switch kind {
		case "sources":
			out[i].Sources = n
		case "facts":
			out[i].Facts = n
		case "experiences":
			out[i].Experiences = n
		}
	}
	return out, rows.Err()
}

// EmbeddedRow is one row's vector plus the labels a scatter plot needs.
type EmbeddedRow struct {
	ID      uuid.UUID
	Title   string
	Kind    string
	Project *string
	Vector  []float32
}

// EmbeddingSample pulls vectors for the embedding map. Rows without an
// embedding are excluded rather than plotted at the origin: they have no
// position, and a cluster at 0,0 would be an artefact of the query
// rather than of the memory.
//
// The vector is cast to text in SQL. pgx returns a pgvector column as
// either bytes or a registered type depending on how the connection was
// set up, and one shape is easier to be sure about than two.
func (s *Store) EmbeddingSample(ctx context.Context, domain string, scope anamnesia.Scope, limit int) ([]EmbeddedRow, error) {
	if limit <= 0 {
		limit = 500
	}
	if limit > 2000 {
		limit = 2000
	}
	var title, kind, table, live string
	switch domain {
	case "experiences":
		table, title, kind, live = "experiences", "t.title", "t.kind", " AND t.deleted_at IS NULL"
	case "facts":
		table, title, kind, live = "facts", "t.key", "t.fact_scope", " AND t.deleted_at IS NULL"
	case "entities":
		table, title, kind = "entities", "t.name", "t.kind"
	default:
		return nil, fmt.Errorf("no embedding map for %q", domain)
	}

	args := []any{scope.UserID}
	where := "t.user_id = $1 AND t.embedding IS NOT NULL" + live
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where += " AND (t.project_id = $2 OR t.project_id IS NULL)"
	}
	args = append(args, limit)
	q := fmt.Sprintf(`SELECT t.id, coalesce(%s, ''), coalesce(%s, ''), p.slug, t.embedding::text
		FROM %s t LEFT JOIN projects p ON p.id = t.project_id
		WHERE %s ORDER BY t.id LIMIT $%d`, title, kind, table, where, len(args))

	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EmbeddedRow{}
	for rows.Next() {
		var (
			r   EmbeddedRow
			raw string
		)
		if err := rows.Scan(&r.ID, &r.Title, &r.Kind, &r.Project, &raw); err != nil {
			return nil, err
		}
		r.Vector = parseVector(raw)
		if len(r.Vector) == 0 {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// parseVector reads pgvector's text form, "[0.1,0.2,...]".
func parseVector(s string) []float32 {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, 0, len(parts))
	for _, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil
		}
		out = append(out, float32(f))
	}
	return out
}
