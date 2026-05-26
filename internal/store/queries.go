// queries.go exposes dynamic-SQL helpers used by the retrieval engine,
// which composes WHERE clauses at runtime. Each helper just runs the SQL
// and scans rows into the package-shared row scanners.
package store

import (
	"context"

	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

// QueryFacts runs an arbitrary SELECT and scans rows into *Fact.
// The SELECT must return the column list expected by scanFact (see
// facts.go).
func (s *Store) QueryFacts(ctx context.Context, sql string, args []any) ([]*anamnesia.Fact, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// QueryExperiences mirrors QueryFacts for experiences.
func (s *Store) QueryExperiences(ctx context.Context, sql string, args []any) ([]*anamnesia.Experience, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Experience
	for rows.Next() {
		e, err := scanExperience(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// QuerySkills mirrors QueryFacts for skills.
func (s *Store) QuerySkills(ctx context.Context, sql string, args []any) ([]*anamnesia.Skill, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
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
