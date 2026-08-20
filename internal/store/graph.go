package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// ─── entities ────────────────────────────────────────────────────────

// UpsertEntity creates or updates an entity keyed by (scope, kind, name).
// Properties are replaced wholesale on conflict; pass the merged map.
func (s *Store) UpsertEntity(ctx context.Context, e *anamnesia.Entity) error {
	if e.Kind == "" || e.Name == "" {
		return errors.New("entity: kind and name required")
	}
	var propsJSON []byte
	if e.Props != nil {
		b, err := json.Marshal(e.Props)
		if err != nil {
			return fmt.Errorf("marshal props: %w", err)
		}
		propsJSON = b
	} else {
		propsJSON = []byte("{}")
	}
	var emb *pgvector.Vector
	if len(e.Embedding) > 0 {
		v := pgvector.NewVector(e.Embedding)
		emb = &v
	}
	return s.Pool.QueryRow(ctx, `
		INSERT INTO entities (user_id, project_id, kind, name, props, embedding)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6)
		ON CONFLICT (user_id, coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid), kind, name)
		DO UPDATE SET
			props     = EXCLUDED.props,
			embedding = COALESCE(EXCLUDED.embedding, entities.embedding)
		RETURNING id, created_at`,
		e.Scope.UserID, e.Scope.ProjectID, e.Kind, e.Name, string(propsJSON), emb,
	).Scan(&e.ID, &e.CreatedAt)
}

// GetEntity returns an entity by id.
func (s *Store) GetEntity(ctx context.Context, id uuid.UUID) (*anamnesia.Entity, error) {
	row := s.Pool.QueryRow(ctx,
		`SELECT id, user_id, project_id, kind, name, props, created_at FROM entities WHERE id = $1`, id)
	return scanEntity(row)
}

// LookupEntity finds an entity by (scope, kind, name).
func (s *Store) LookupEntity(ctx context.Context, scope anamnesia.Scope, kind, name string) (*anamnesia.Entity, error) {
	row := s.Pool.QueryRow(ctx, `
		SELECT id, user_id, project_id, kind, name, props, created_at
		FROM entities
		WHERE user_id = $1
		  AND coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid)
		      = coalesce($2, '00000000-0000-0000-0000-000000000000'::uuid)
		  AND kind = $3 AND name = $4`,
		scope.UserID, scope.ProjectID, kind, name)
	return scanEntity(row)
}

// LookupEntitiesByName finds every entity in scope with the given
// (already normalised) name, across all kinds. Unlike LookupEntity this
// takes no kind, because a bare edge endpoint name doesn't carry one —
// the caller decides what to do when more than one entity matches: an
// edge endpoint is ambiguous, not resolvable to either.
func (s *Store) LookupEntitiesByName(ctx context.Context, scope anamnesia.Scope, name string) ([]*anamnesia.Entity, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, user_id, project_id, kind, name, props, created_at
		FROM entities
		WHERE user_id = $1
		  AND coalesce(project_id, '00000000-0000-0000-0000-000000000000'::uuid)
		      = coalesce($2, '00000000-0000-0000-0000-000000000000'::uuid)
		  AND name = $3`,
		scope.UserID, scope.ProjectID, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Entity
	for rows.Next() {
		ent, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ent)
	}
	return out, rows.Err()
}

// ListEntities returns recent entities in scope.
func (s *Store) ListEntities(ctx context.Context, scope anamnesia.Scope, kind string, limit int) ([]*anamnesia.Entity, error) {
	if limit <= 0 {
		limit = 50
	}
	args := []any{scope.UserID}
	where := []string{"user_id = $1"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("project_id = $%d", len(args)))
	}
	if kind != "" {
		args = append(args, kind)
		where = append(where, fmt.Sprintf("kind = $%d", len(args)))
	}
	args = append(args, limit)
	q := fmt.Sprintf(`SELECT id, user_id, project_id, kind, name, props, created_at
		FROM entities WHERE %s ORDER BY created_at DESC LIMIT $%d`,
		strings.Join(where, " AND "), len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Entity
	for rows.Next() {
		ent, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ent)
	}
	return out, rows.Err()
}

// EntitiesMissingEmbedding returns entities without an embedding yet.
func (s *Store) EntitiesMissingEmbedding(ctx context.Context, n int) ([]*anamnesia.Entity, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT id, user_id, project_id, kind, name, props, created_at
		 FROM entities WHERE embedding IS NULL
		 ORDER BY created_at ASC LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Entity
	for rows.Next() {
		ent, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ent)
	}
	return out, rows.Err()
}

// SetEntityEmbedding writes a vector to an entity row.
func (s *Store) SetEntityEmbedding(ctx context.Context, id uuid.UUID, vec []float32) error {
	v := pgvector.NewVector(vec)
	_, err := s.Pool.Exec(ctx, `UPDATE entities SET embedding = $2 WHERE id = $1`, id, v)
	return err
}

// EntityMatch pairs an entity with its cosine distance from a probe vector.
type EntityMatch struct {
	Entity   *anamnesia.Entity
	Distance float64
}

// NearestEntities returns the entities in scope closest to vec by cosine
// distance, nearest first. Entities without an embedding are skipped. The
// caller decides what distance counts as close enough; no threshold is
// applied here.
func (s *Store) NearestEntities(ctx context.Context, scope anamnesia.Scope, vec []float32, limit int) ([]EntityMatch, error) {
	args := []any{scope.UserID, pgvector.NewVector(vec)}
	where := []string{"user_id = $1", "embedding IS NOT NULL"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("(project_id = $%d OR project_id IS NULL)", len(args)))
	}
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT id, user_id, project_id, kind, name, props, created_at, embedding <=> $2 AS distance
		FROM entities WHERE %s
		ORDER BY embedding <=> $2 ASC
		LIMIT $%d`, strings.Join(where, " AND "), len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntityMatch
	for rows.Next() {
		var (
			e        anamnesia.Entity
			project  *uuid.UUID
			propsRaw []byte
			distance float64
		)
		if err := rows.Scan(&e.ID, &e.Scope.UserID, &project, &e.Kind, &e.Name, &propsRaw, &e.CreatedAt, &distance); err != nil {
			return nil, err
		}
		e.Scope.ProjectID = project
		if len(propsRaw) > 0 {
			_ = json.Unmarshal(propsRaw, &e.Props)
		}
		out = append(out, EntityMatch{Entity: &e, Distance: distance})
	}
	return out, rows.Err()
}

func scanEntity(row rowScanner) (*anamnesia.Entity, error) {
	var (
		e        anamnesia.Entity
		project  *uuid.UUID
		propsRaw []byte
	)
	err := row.Scan(&e.ID, &e.Scope.UserID, &project, &e.Kind, &e.Name, &propsRaw, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.Scope.ProjectID = project
	if len(propsRaw) > 0 {
		_ = json.Unmarshal(propsRaw, &e.Props)
	}
	return &e, nil
}

// ─── edges ───────────────────────────────────────────────────────────

// CreateEdge inserts a new edge. Callers should set ValidFrom (defaults
// to now()). Use InvalidateEdge to close a bitemporal interval.
func (s *Store) CreateEdge(ctx context.Context, e *anamnesia.Edge) error {
	if e.From == uuid.Nil || e.To == uuid.Nil || e.Kind == "" {
		return errors.New("edge: from, to, kind required")
	}
	if e.Trust == 0 {
		e.Trust = 0.5
	}
	if e.Source == "" {
		e.Source = "system"
	}
	if e.ValidFrom.IsZero() {
		e.ValidFrom = time.Now().UTC()
	}
	var propsJSON []byte
	if e.Props != nil {
		b, err := json.Marshal(e.Props)
		if err != nil {
			return fmt.Errorf("marshal props: %w", err)
		}
		propsJSON = b
	} else {
		propsJSON = []byte("{}")
	}
	return s.Pool.QueryRow(ctx, `
		INSERT INTO edges (from_id, to_id, kind, props, valid_from, valid_to, source, trust)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8)
		RETURNING id, ingested_at`,
		e.From, e.To, e.Kind, string(propsJSON), e.ValidFrom, e.ValidTo, e.Source, e.Trust,
	).Scan(&e.ID, &e.IngestedAt)
}

// InvalidateEdge closes an edge: stamps invalidated_at and (optionally)
// valid_to. Use when a relation is no longer true.
func (s *Store) InvalidateEdge(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE edges SET invalidated_at = now(), valid_to = COALESCE(valid_to, now())
		WHERE id = $1 AND invalidated_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Neighbors returns the entities reachable from src by edges of the given
// kinds (empty = all kinds), with their connecting edges. Direction:
// "out" follows from→to, "in" follows to→from, "both" walks both ways.
// Only currently-valid edges count (invalidated_at IS NULL and
// valid_to IS NULL OR > now()).
func (s *Store) Neighbors(ctx context.Context, src uuid.UUID, kinds []string, direction string, limit int) ([]*anamnesia.Entity, []*anamnesia.Edge, error) {
	if limit <= 0 {
		limit = 50
	}
	dir := direction
	if dir == "" {
		dir = "out"
	}
	var (
		entWhere string
		edgeJoin string
	)
	switch dir {
	case "out":
		entWhere = `e.from_id = $1`
		edgeJoin = `ent.id = e.to_id`
	case "in":
		entWhere = `e.to_id = $1`
		edgeJoin = `ent.id = e.from_id`
	case "both":
		entWhere = `(e.from_id = $1 OR e.to_id = $1)`
		edgeJoin = `(ent.id = e.to_id OR ent.id = e.from_id) AND ent.id <> $1`
	default:
		return nil, nil, fmt.Errorf("direction must be out|in|both, got %q", dir)
	}
	args := []any{src}
	if len(kinds) > 0 {
		args = append(args, kinds)
		entWhere += fmt.Sprintf(" AND e.kind = ANY($%d)", len(args))
	}
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT DISTINCT
		       ent.id, ent.user_id, ent.project_id, ent.kind, ent.name, ent.props, ent.created_at,
		       e.id, e.from_id, e.to_id, e.kind, e.props, e.valid_from, e.valid_to,
		       e.ingested_at, e.invalidated_at, e.source, e.trust
		  FROM edges e
		  JOIN entities ent ON %s
		 WHERE %s
		   AND e.invalidated_at IS NULL
		   AND (e.valid_to IS NULL OR e.valid_to > now())
		 LIMIT $%d`, edgeJoin, entWhere, len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var (
		ents  []*anamnesia.Entity
		edges []*anamnesia.Edge
	)
	for rows.Next() {
		var (
			ent       anamnesia.Entity
			project   *uuid.UUID
			propsRaw  []byte
			edge      anamnesia.Edge
			edgeProps []byte
			validTo   *time.Time
			invalid   *time.Time
		)
		if err := rows.Scan(
			&ent.ID, &ent.Scope.UserID, &project, &ent.Kind, &ent.Name, &propsRaw, &ent.CreatedAt,
			&edge.ID, &edge.From, &edge.To, &edge.Kind, &edgeProps, &edge.ValidFrom, &validTo,
			&edge.IngestedAt, &invalid, &edge.Source, &edge.Trust,
		); err != nil {
			return nil, nil, err
		}
		ent.Scope.ProjectID = project
		if len(propsRaw) > 0 {
			_ = json.Unmarshal(propsRaw, &ent.Props)
		}
		if len(edgeProps) > 0 {
			_ = json.Unmarshal(edgeProps, &edge.Props)
		}
		edge.ValidTo = validTo
		edge.InvalidatedAt = invalid
		ents = append(ents, &ent)
		edges = append(edges, &edge)
	}
	return ents, edges, rows.Err()
}

// ─── entity_mentions ─────────────────────────────────────────────────

// RecordMention notes that a source mentioned an entity. Idempotent: a
// re-extraction of the same source must not error.
func (s *Store) RecordMention(ctx context.Context, entityID, sourceID uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO entity_mentions (entity_id, source_id)
		VALUES ($1, $2)
		ON CONFLICT (entity_id, source_id) DO NOTHING`, entityID, sourceID)
	if err != nil {
		return fmt.Errorf("record mention: %w", err)
	}
	return nil
}

// EntitiesForSources returns the entities those sources mentioned. This is
// the outward half of the bridge: a search hit knows its source, and this
// turns that into somewhere to start walking.
func (s *Store) EntitiesForSources(ctx context.Context, sourceIDs []uuid.UUID) ([]*anamnesia.Entity, error) {
	if len(sourceIDs) == 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT e.id, e.user_id, e.project_id, e.kind, e.name, e.props, e.created_at
		  FROM entities e
		  JOIN entity_mentions m ON m.entity_id = e.id
		 WHERE m.source_id = ANY($1)`, sourceIDs)
	if err != nil {
		return nil, fmt.Errorf("entities for sources: %w", err)
	}
	defer rows.Close()
	var out []*anamnesia.Entity
	for rows.Next() {
		ent, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ent)
	}
	return out, rows.Err()
}

// EntitySource is one entity_mentions row: an entity, and a source that
// mentioned it. SourcesForEntities returns these rather than a flat list
// of source ids so a caller batching many entities into one call can
// still tell which entity reached which source — the graph walk ranks a
// reachable source by the trust of the edge that got to the entity
// mentioning it (internal/retrieval/graph.go), and that association is
// the only thing carrying that trust across the batch.
type EntitySource struct {
	EntityID uuid.UUID
	SourceID uuid.UUID
}

// SourcesForEntities returns the sources that mentioned those entities. The
// inward half: having walked to a neighbour, this is how its memory rows are
// reached, since facts and experiences carry source_id.
//
// One row per (entity, source) pair, which entity_mentions' primary key
// already makes unique. A source mentioned by several of the entities
// asked about therefore appears once per entity, not once in total.
func (s *Store) SourcesForEntities(ctx context.Context, entityIDs []uuid.UUID) ([]EntitySource, error) {
	if len(entityIDs) == 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT entity_id, source_id FROM entity_mentions WHERE entity_id = ANY($1)`, entityIDs)
	if err != nil {
		return nil, fmt.Errorf("sources for entities: %w", err)
	}
	defer rows.Close()
	var out []EntitySource
	for rows.Next() {
		var es EntitySource
		if err := rows.Scan(&es.EntityID, &es.SourceID); err != nil {
			return nil, err
		}
		out = append(out, es)
	}
	return out, rows.Err()
}
