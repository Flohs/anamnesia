// queries.go exposes dynamic-SQL helpers used by the retrieval engine,
// which composes WHERE clauses at runtime. Each helper just runs the SQL
// and scans rows into the package-shared row scanners.
package store

import (
	"context"

	"github.com/flohs/anamnesia/pkg/anamnesia"
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

// ScoredFact and ScoredExperience are a row with the cosine distance
// that found it, the way ScoredArtifact already is for artifacts. The
// distance is the only absolute number a search produces, so the vector
// channel has to hand it back rather than discard it once it has sorted
// by it.
type ScoredFact struct {
	Fact     *anamnesia.Fact
	Distance float64
}

type ScoredExperience struct {
	Experience *anamnesia.Experience
	Distance   float64
}

// withDistance lets a scored query reuse the shared row scanners: it
// appends one trailing scan target, so scanFact and scanExperience keep
// owning their own column lists and no query has to repeat them.
type withDistance struct {
	row  rowScanner
	dist *float64
}

func (w withDistance) Scan(dest ...any) error { return w.row.Scan(append(dest, w.dist)...) }

// QueryScoredFacts runs a SELECT whose columns are scanFact's followed by
// a distance.
func (s *Store) QueryScoredFacts(ctx context.Context, sql string, args []any) ([]ScoredFact, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoredFact
	for rows.Next() {
		var d float64
		f, err := scanFact(withDistance{rows, &d})
		if err != nil {
			return nil, err
		}
		out = append(out, ScoredFact{Fact: f, Distance: d})
	}
	return out, rows.Err()
}

// QueryScoredExperiences mirrors QueryScoredFacts for experiences.
func (s *Store) QueryScoredExperiences(ctx context.Context, sql string, args []any) ([]ScoredExperience, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoredExperience
	for rows.Next() {
		var d float64
		e, err := scanExperience(withDistance{rows, &d})
		if err != nil {
			return nil, err
		}
		out = append(out, ScoredExperience{Experience: e, Distance: d})
	}
	return out, rows.Err()
}
