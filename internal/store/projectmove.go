// projectmove.go: refiling one project's memories under another slug.
//
// A project slug is derived from a repository directory unless
// .anamnesia.toml says otherwise, so one product split across several
// repositories becomes several projects. That is invisible until it
// matters: the read path scopes to `project_id = $n OR project_id IS
// NULL`, so work in one repository cannot see decisions recorded in
// another, and consolidation clusters strictly within a scope.
//
// Moving is a plain re-parenting of every row that names the project.
// The only difficulty is the three unique indexes that include
// project_id: two projects may each hold a `db.host`, and only one of
// them can survive in the target. Rather than pick a winner, a move that
// would collide is refused with the colliding names, because discarding
// a fact the user never chose to lose is not a repair.

package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// projectScopedTables is every table carrying a project_id. A table
// missing here is not a loud failure: its rows would simply stay behind
// under a slug nothing files under any more, so
// TestTheMoverCoversEveryTableThatNamesAProject checks this against the
// live schema rather than trusting the list.
var projectScopedTables = []string{
	"audit_log",
	"commitments",
	"entities",
	"experiences",
	"facts",
	"skills",
	"sources",
	"working_memory",
}

// ProjectMovePlan is what a move would do, and why it might not be
// allowed to. Producing one writes nothing.
type ProjectMovePlan struct {
	From, To string
	FromID   uuid.UUID
	ToID     uuid.UUID // uuid.Nil when the target does not exist yet
	Found    bool      // the source project exists at all
	Counts   map[string]int

	// Blocking conditions, each named rather than counted so the message
	// tells the user which row to deal with.
	FactKeys   []string
	SkillNames []string
	Entities   int
}

// Total is how many rows would move.
func (p *ProjectMovePlan) Total() int {
	n := 0
	for _, c := range p.Counts {
		n += c
	}
	return n
}

// Blockers explains, in the user's terms, why this move cannot proceed.
func (p *ProjectMovePlan) Blockers() []string {
	var out []string
	if len(p.FactKeys) > 0 {
		out = append(out, fmt.Sprintf(
			"%d fact key(s) exist in both projects and only one can survive: %v",
			len(p.FactKeys), p.FactKeys))
	}
	if len(p.SkillNames) > 0 {
		out = append(out, fmt.Sprintf(
			"%d skill name(s) exist in both projects: %v", len(p.SkillNames), p.SkillNames))
	}
	if p.Entities > 0 {
		out = append(out, fmt.Sprintf(
			"%d entity row(s) in %q: merging entities means repointing every edge at the survivor, which this command does not do yet",
			p.Entities, p.From))
	}
	return out
}

// PlanProjectMove reports what moving `from` into `to` would do. It
// writes nothing, including not creating the target project.
func (s *Store) PlanProjectMove(ctx context.Context, userID uuid.UUID, from, to string) (*ProjectMovePlan, error) {
	if from == to {
		return nil, fmt.Errorf("%q is already the project; nothing to move", from)
	}
	p := &ProjectMovePlan{From: from, To: to, Counts: map[string]int{}}

	fromID, found, err := s.LookupProject(ctx, userID, from)
	if err != nil {
		return nil, err
	}
	p.Found, p.FromID = found, fromID
	if !found {
		// Not an error: a repository whose sessions have not been
		// checkpointed yet has no project row, and refiling it is still
		// a legitimate thing to ask for.
		return p, nil
	}

	for _, tbl := range projectScopedTables {
		var n int
		if err := s.Pool.QueryRow(ctx,
			`SELECT count(*) FROM `+tbl+` WHERE user_id=$1 AND project_id=$2`,
			userID, fromID).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", tbl, err)
		}
		if n > 0 {
			p.Counts[tbl] = n
		}
	}
	p.Entities = p.Counts["entities"]

	toID, toFound, err := s.LookupProject(ctx, userID, to)
	if err != nil {
		return nil, err
	}
	if !toFound {
		// Nothing to collide with.
		return p, nil
	}
	p.ToID = toID

	if p.FactKeys, err = s.collidingFactKeys(ctx, userID, fromID, toID); err != nil {
		return nil, err
	}
	if p.SkillNames, err = s.collidingSkillNames(ctx, userID, fromID, toID); err != nil {
		return nil, err
	}
	return p, nil
}

// collidingFactKeys finds keys live in both projects. The predicate
// mirrors facts_identity: only rows that are in the index can collide, so
// a deleted or superseded row in the target is not an obstacle.
func (s *Store) collidingFactKeys(ctx context.Context, userID, fromID, toID uuid.UUID) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT a.key FROM facts a
		JOIN facts b
		  ON b.user_id = a.user_id AND b.project_id = $3
		 AND b.fact_scope = a.fact_scope AND b.key = a.key
		 AND b.deleted_at IS NULL AND b.superseded_by IS NULL
		WHERE a.user_id = $1 AND a.project_id = $2
		  AND a.deleted_at IS NULL AND a.superseded_by IS NULL
		ORDER BY 1`, userID, fromID, toID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrings(rows)
}

func (s *Store) collidingSkillNames(ctx context.Context, userID, fromID, toID uuid.UUID) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT a.name FROM skills a
		JOIN skills b
		  ON b.user_id = a.user_id AND b.project_id = $3 AND b.name = a.name
		 AND b.deleted_at IS NULL
		WHERE a.user_id = $1 AND a.project_id = $2 AND a.deleted_at IS NULL
		ORDER BY 1`, userID, fromID, toID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrings(rows)
}

func scanStrings(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]string, error) {
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// ApplyProjectMove re-parents every row in one transaction, so a failure
// part-way leaves the project whole rather than split across two slugs.
func (s *Store) ApplyProjectMove(ctx context.Context, userID uuid.UUID, plan *ProjectMovePlan) error {
	if blockers := plan.Blockers(); len(blockers) > 0 {
		return fmt.Errorf("refusing to move %q into %q: %v", plan.From, plan.To, blockers)
	}
	if !plan.Found || plan.Total() == 0 {
		return nil
	}
	// Created outside the transaction because EnsureProject has its own;
	// an empty project left behind by a later failure is inert.
	toID, err := s.EnsureProject(ctx, userID, plan.To)
	if err != nil {
		return fmt.Errorf("ensure %q: %w", plan.To, err)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, tbl := range projectScopedTables {
		if _, err := tx.Exec(ctx,
			`UPDATE `+tbl+` SET project_id=$1 WHERE user_id=$2 AND project_id=$3`,
			toID, userID, plan.FromID); err != nil {
			return fmt.Errorf("move %s: %w", tbl, err)
		}
	}
	return tx.Commit(ctx)
}
