// directory.go lists the users and projects memory is filed under, with
// enough of a summary to answer "where does memory accumulate, and when
// was this project last touched".
//
// The counts are correlated subqueries rather than joins: a project has
// five independent one-to-many relationships, and joining them all
// multiplies the rows before it counts them.
package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// DomainCounts is how many rows of each kind belong somewhere.
type DomainCounts struct {
	Facts       int `json:"facts"`
	Experiences int `json:"experiences"`
	Skills      int `json:"skills"`
	Entities    int `json:"entities"`
	Sources     int `json:"sources"`
}

// ProjectSummary is one project with its counts.
type ProjectSummary struct {
	ID           uuid.UUID
	Slug         string
	UserID       uuid.UUID
	User         string
	CreatedAt    time.Time
	LastActivity *time.Time
	Counts       DomainCounts
}

// UserSummary is one user with their counts.
type UserSummary struct {
	ID           uuid.UUID
	Handle       string
	CreatedAt    time.Time
	LastActivity *time.Time
	Projects     int
	Counts       DomainCounts
}

// countsFor builds the five count subqueries against one owning column.
func countsFor(column string) string {
	return `
	  (SELECT count(*) FROM facts       x WHERE x.` + column + ` = o.id AND x.deleted_at IS NULL AND x.superseded_by IS NULL),
	  (SELECT count(*) FROM experiences x WHERE x.` + column + ` = o.id AND x.deleted_at IS NULL),
	  (SELECT count(*) FROM skills      x WHERE x.` + column + ` = o.id AND x.deleted_at IS NULL),
	  (SELECT count(*) FROM entities    x WHERE x.` + column + ` = o.id),
	  (SELECT count(*) FROM sources     x WHERE x.` + column + ` = o.id),
	  GREATEST(
	    (SELECT max(x.ingested_at) FROM facts       x WHERE x.` + column + ` = o.id),
	    (SELECT max(x.ingested_at) FROM experiences x WHERE x.` + column + ` = o.id),
	    (SELECT max(x.ingested_at) FROM sources     x WHERE x.` + column + ` = o.id)
	  )`
}

// ListProjects returns every project, or one user's, newest activity
// first. A project nothing has ever been written to sorts last rather
// than disappearing: an empty project is a state worth seeing.
func (s *Store) ListProjects(ctx context.Context, userID *uuid.UUID) ([]ProjectSummary, error) {
	q := `SELECT o.id, o.slug, o.user_id, u.handle, o.created_at,` + countsFor("project_id") + `
	      FROM projects o JOIN users u ON u.id = o.user_id`
	var args []any
	if userID != nil {
		q += ` WHERE o.user_id = $1`
		args = append(args, *userID)
	}
	q += ` ORDER BY 11 DESC NULLS LAST, o.slug ASC`

	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectSummary{}
	for rows.Next() {
		var p ProjectSummary
		if err := rows.Scan(&p.ID, &p.Slug, &p.UserID, &p.User, &p.CreatedAt,
			&p.Counts.Facts, &p.Counts.Experiences, &p.Counts.Skills,
			&p.Counts.Entities, &p.Counts.Sources, &p.LastActivity); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListUsers returns every user with their counts, newest activity first.
func (s *Store) ListUsers(ctx context.Context) ([]UserSummary, error) {
	q := `SELECT o.id, o.handle, o.created_at,
	        (SELECT count(*) FROM projects x WHERE x.user_id = o.id),` + countsFor("user_id") + `
	      FROM users o
	      ORDER BY 10 DESC NULLS LAST, o.handle ASC`
	rows, err := s.Pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserSummary{}
	for rows.Next() {
		var u UserSummary
		if err := rows.Scan(&u.ID, &u.Handle, &u.CreatedAt, &u.Projects,
			&u.Counts.Facts, &u.Counts.Experiences, &u.Counts.Skills,
			&u.Counts.Entities, &u.Counts.Sources, &u.LastActivity); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
