// projectprune.go: removing project rows that hold nothing.
//
// A project used to be created by reading, so opening a directory filed
// it whether or not anything was ever stored there. That is fixed at the
// source, but the rows it already made are still here. This is the
// cleanup, and it is deliberately a command rather than a worker: the
// only cost of an empty project is that it shows up in a list, and a
// background process that deletes rows on its own to tidy a display is a
// poor trade. It would also be futile, since reading a project recreates
// it the moment the directory is opened again.

package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// EmptyProject is a project holding no rows in any table that names it.
type EmptyProject struct {
	ID   uuid.UUID
	Slug string
}

// PrunableProjects lists this user's projects that hold nothing at all.
//
// "Nothing" is checked against projectScopedTables, the same list the
// mover uses and the one TestTheMoverCoversEveryTableThatNamesAProject
// holds against the live schema. A table missing from it would make this
// delete a project that still has rows in it.
func (s *Store) PrunableProjects(ctx context.Context, userID uuid.UUID) ([]EmptyProject, error) {
	q := `SELECT p.id, p.slug FROM projects p WHERE p.user_id = $1`
	for _, tbl := range projectScopedTables {
		q += fmt.Sprintf(" AND NOT EXISTS (SELECT 1 FROM %s t WHERE t.project_id = p.id)", tbl)
	}
	q += " ORDER BY p.slug"
	rows, err := s.Pool.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EmptyProject
	for rows.Next() {
		var p EmptyProject
		if err := rows.Scan(&p.ID, &p.Slug); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PruneProjects deletes the named projects, re-checking emptiness inside
// the transaction.
//
// The re-check is the point. The listing a user approves is a snapshot,
// and a session can checkpoint into one of those projects between the
// dry run and the apply. Deleting on the strength of the earlier read
// would take real memory with it.
func (s *Store) PruneProjects(ctx context.Context, userID uuid.UUID, slugs []string) (int, error) {
	if len(slugs) == 0 {
		return 0, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	deleted := 0
	for _, slug := range slugs {
		var id uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT id FROM projects WHERE user_id=$1 AND slug=$2`, userID, slug).Scan(&id)
		if err != nil {
			// Already gone is not a failure; anything else is.
			if err.Error() == "no rows in result set" {
				continue
			}
			return 0, fmt.Errorf("look up %q: %w", slug, err)
		}
		for _, tbl := range projectScopedTables {
			var n int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM `+tbl+` WHERE project_id = $1`, id).Scan(&n); err != nil {
				return 0, fmt.Errorf("check %s for %q: %w", tbl, slug, err)
			}
			if n > 0 {
				return 0, fmt.Errorf("%q now holds %d row(s) in %s and is no longer empty; nothing was deleted", slug, n, tbl)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM projects WHERE id=$1`, id); err != nil {
			return 0, fmt.Errorf("delete %q: %w", slug, err)
		}
		deleted++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return deleted, nil
}
