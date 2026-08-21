// stats.go counts what is in the database, for the read API.
//
// These are the numbers an observer needs to tell an install with
// nothing in it from one that is failing to put anything in it: how many
// rows exist per domain, how many sources never made it through the
// extractor, and how much of what exists carries an embedding. Vector
// search returning nothing has exactly two causes, and embedding
// coverage separates them.
package store

import (
	"context"
	"strings"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// Coverage is how many rows of a domain exist and how many of those
// carry an embedding.
type Coverage struct {
	Total    int `json:"total"`
	Embedded int `json:"embedded"`
}

// StatsResult is the whole count set for one scope. Users and Projects
// are server-wide; everything else respects the scope.
type StatsResult struct {
	Users    int
	Projects int

	Facts         int
	Experiences   int
	Skills        int
	WorkingMemory int
	Entities      int
	Edges         int
	Sources       int
	Commitments   int

	SourcesByState           map[string]int
	ExperiencesByAbstraction map[int]int
	EmbeddingCoverage        map[string]Coverage

	ExtractPending int
	EmbedPending   int
}

// scopeWhere builds the standard scope predicate: everything belonging
// to the user, narrowed to one project plus its user-level rows when a
// project is set. It matches what retrieval and the List* methods do, so
// a browse and a search agree about what is in scope.
func scopeWhere(scope anamnesia.Scope, projectColumn bool) (string, []any) {
	args := []any{scope.UserID}
	where := "user_id = $1"
	if projectColumn && scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where += " AND (project_id = $2 OR project_id IS NULL)"
	}
	return where, args
}

// prefixColumns qualifies the scope predicate's columns for a joined
// query. The predicate is built here and never from user input, so this
// is string work on a known shape rather than SQL assembly.
func prefixColumns(where, prefix string) string {
	where = strings.ReplaceAll(where, "user_id", prefix+"user_id")
	return strings.ReplaceAll(where, "project_id", prefix+"project_id")
}

// Stats counts every domain in one scope.
func (s *Store) Stats(ctx context.Context, scope anamnesia.Scope) (*StatsResult, error) {
	out := &StatsResult{
		SourcesByState:           map[string]int{},
		ExperiencesByAbstraction: map[int]int{},
		EmbeddingCoverage:        map[string]Coverage{},
	}

	if err := s.Pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM projects)`,
	).Scan(&out.Users, &out.Projects); err != nil {
		return nil, err
	}

	where, args := scopeWhere(scope, true)
	counts := []struct {
		table string
		live  string
		into  *int
	}{
		{"facts", "deleted_at IS NULL AND superseded_by IS NULL", &out.Facts},
		{"experiences", "deleted_at IS NULL", &out.Experiences},
		{"skills", "deleted_at IS NULL", &out.Skills},
		{"working_memory", "", &out.WorkingMemory},
		{"entities", "", &out.Entities},
		{"sources", "", &out.Sources},
		{"commitments", "", &out.Commitments},
	}
	for _, c := range counts {
		q := "SELECT count(*) FROM " + c.table + " WHERE " + where
		if c.live != "" {
			q += " AND " + c.live
		}
		if err := s.Pool.QueryRow(ctx, q, args...).Scan(c.into); err != nil {
			return nil, err
		}
	}

	// Edges carry no scope of their own: they hang off entities, which
	// do, so scoping one means joining to the entity it starts from.
	if err := s.Pool.QueryRow(ctx,
		"SELECT count(*) FROM edges e JOIN entities en ON en.id = e.from_id WHERE "+
			prefixColumns(where, "en.")+" AND e.invalidated_at IS NULL",
		args...,
	).Scan(&out.Edges); err != nil {
		return nil, err
	}

	rows, err := s.Pool.Query(ctx,
		"SELECT extraction_state, count(*) FROM sources WHERE "+where+" GROUP BY extraction_state", args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			rows.Close()
			return nil, err
		}
		out.SourcesByState[state] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Every state is reported, including the ones at zero: "0 failed" is
	// a different statement from "no failed key in the response".
	for _, state := range []string{
		anamnesia.ExtractionPending, anamnesia.ExtractionDone,
		anamnesia.ExtractionFailed, anamnesia.ExtractionSkipped,
	} {
		if _, ok := out.SourcesByState[state]; !ok {
			out.SourcesByState[state] = 0
		}
	}

	rows, err = s.Pool.Query(ctx,
		"SELECT abstraction, count(*) FROM experiences WHERE "+where+
			" AND deleted_at IS NULL GROUP BY abstraction ORDER BY abstraction", args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var level, n int
		if err := rows.Scan(&level, &n); err != nil {
			rows.Close()
			return nil, err
		}
		out.ExperiencesByAbstraction[level] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, domain := range []string{"facts", "experiences", "entities"} {
		var cov Coverage
		q := "SELECT count(*), count(embedding) FROM " + domain + " WHERE " + where
		if domain != "entities" {
			q += " AND deleted_at IS NULL"
		}
		if err := s.Pool.QueryRow(ctx, q, args...).Scan(&cov.Total, &cov.Embedded); err != nil {
			return nil, err
		}
		out.EmbeddingCoverage[domain] = cov
	}

	extract, embed, err := s.QueuePending(ctx, scope.UserID)
	if err != nil {
		return nil, err
	}
	out.ExtractPending, out.EmbedPending = extract, embed
	return out, nil
}

// QueuePendingAll is QueuePending without a user: the server-wide view
// the activity screen shows, where the question is whether the worker is
// keeping up rather than whose memory is waiting.
func (s *Store) QueuePendingAll(ctx context.Context) (extract, embed int, err error) {
	err = s.Pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM sources WHERE extraction_state = 'pending'),
		  (SELECT count(*) FROM facts WHERE embedding IS NULL AND deleted_at IS NULL)
		  + (SELECT count(*) FROM experiences WHERE embedding IS NULL AND deleted_at IS NULL)
		  + (SELECT count(*) FROM entities WHERE embedding IS NULL)
	`).Scan(&extract, &embed)
	return extract, embed, err
}
