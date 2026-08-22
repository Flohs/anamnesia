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

// artifactCols is the column list every artifact read shares, so a scan
// and its query cannot drift apart.
const artifactCols = `artifacts.id, artifacts.user_id, artifacts.project_id, artifacts.artifact_uuid,
	artifacts.url, artifacts.title, artifacts.description, artifacts.file_path, artifacts.body,
	artifacts.meta, artifacts.embed_model, artifacts.occurred_at, artifacts.created_at,
	artifacts.updated_at, artifacts.deleted_at`

// UpsertArtifact records a published artifact, keyed by its own UUID.
//
// Two fields are deliberately not overwritten by a later write. project_id
// stays as first published: an artifact belongs to the work that made it,
// and republishing from another repository should not refile it. body is
// kept when the incoming one is empty, because the backfill reads
// transcripts long after the source file was cleaned up, and running it
// must never blank out a body the live hook captured while the file
// still existed.
func (s *Store) UpsertArtifact(ctx context.Context, a *anamnesia.Artifact) error {
	if a.ArtifactUUID == uuid.Nil {
		return errors.New("artifact: artifact_uuid required")
	}
	if a.URL == "" {
		return errors.New("artifact: url required")
	}
	if a.OccurredAt.IsZero() {
		a.OccurredAt = time.Now().UTC()
	}
	var metaJSON []byte
	if a.Meta != nil {
		b, err := json.Marshal(a.Meta)
		if err != nil {
			return fmt.Errorf("marshal meta: %w", err)
		}
		metaJSON = b
	}
	var emb *pgvector.Vector
	if len(a.Embedding) > 0 {
		v := pgvector.NewVector(a.Embedding)
		emb = &v
	}
	return s.Tx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO artifacts (user_id, project_id, artifact_uuid, url, title, description,
			                       file_path, body, meta, embedding, embed_model, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12)
			ON CONFLICT (user_id, artifact_uuid) WHERE deleted_at IS NULL
			DO UPDATE SET
				url         = EXCLUDED.url,
				title       = coalesce(EXCLUDED.title, artifacts.title),
				description = coalesce(EXCLUDED.description, artifacts.description),
				file_path   = coalesce(EXCLUDED.file_path, artifacts.file_path),
				body        = coalesce(EXCLUDED.body, artifacts.body),
				meta        = coalesce(EXCLUDED.meta, artifacts.meta),
				occurred_at = EXCLUDED.occurred_at,
				updated_at  = now(),
				-- A changed body has to be re-embedded, and the worker
				-- picks up exactly the rows whose embedding is NULL.
				embedding   = CASE WHEN EXCLUDED.body IS DISTINCT FROM artifacts.body
				                   THEN NULL ELSE artifacts.embedding END,
				embed_model = CASE WHEN EXCLUDED.body IS DISTINCT FROM artifacts.body
				                   THEN NULL ELSE artifacts.embed_model END
			RETURNING id, created_at, updated_at`,
			a.Scope.UserID, a.Scope.ProjectID, a.ArtifactUUID, a.URL,
			nilOrString(a.Title), nilOrString(a.Description),
			nilOrString(a.FilePath), nilOrString(a.Body), nilOrJSON(metaJSON),
			emb, nilOrString(a.EmbedModel), a.OccurredAt,
		).Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt)
	})
}

// ListArtifacts returns artifacts in scope, newest first. A nil ProjectID
// means every project, which is what `anamnesia artifacts` shows without
// --project.
func (s *Store) ListArtifacts(ctx context.Context, scope anamnesia.Scope, limit int) ([]*anamnesia.Artifact, error) {
	if limit <= 0 {
		limit = 50
	}
	args := []any{scope.UserID}
	where := []string{"artifacts.user_id = $1", "artifacts.deleted_at IS NULL"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("artifacts.project_id = $%d", len(args)))
	}
	args = append(args, limit)
	// Joined to projects so a listing can name the project an artifact
	// came from. Without it the CLI could only print a uuid, which is
	// the one thing a person cannot read.
	q := fmt.Sprintf(`SELECT %s, coalesce(projects.slug, '')
		FROM artifacts LEFT JOIN projects ON projects.id = artifacts.project_id
		WHERE %s ORDER BY artifacts.occurred_at DESC LIMIT $%d`,
		artifactCols, strings.Join(where, " AND "), len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Artifact
	for rows.Next() {
		a, err := scanArtifactWithProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// QueryArtifacts runs an arbitrary artifact query, for the retrieval
// engine's channels.
func (s *Store) QueryArtifacts(ctx context.Context, sql string, args []any) ([]*anamnesia.Artifact, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ArtifactColumns exposes the shared column list to callers building
// their own artifact queries.
func ArtifactColumns() string { return artifactCols }

// ScoredArtifact is an artifact with the cosine distance that found it.
// Distance is what makes an absolute relevance decision possible; a
// fused rank cannot, because the best of an irrelevant pool ranks first
// exactly as the best of a relevant one does.
type ScoredArtifact struct {
	Artifact *anamnesia.Artifact
	Distance float64
}

// QueryScoredArtifacts runs a query whose final column is a distance.
func (s *Store) QueryScoredArtifacts(ctx context.Context, sql string, args []any) ([]ScoredArtifact, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScoredArtifact
	for rows.Next() {
		var (
			a       anamnesia.Artifact
			project *uuid.UUID
			strs    [5]*string
			meta    []byte
			deleted *time.Time
			dist    float64
		)
		if err := rows.Scan(
			&a.ID, &a.Scope.UserID, &project, &a.ArtifactUUID, &a.URL, &strs[0], &strs[1],
			&strs[2], &strs[3], &meta, &strs[4],
			&a.OccurredAt, &a.CreatedAt, &a.UpdatedAt, &deleted, &dist,
		); err != nil {
			return nil, err
		}
		a.Scope.ProjectID = project
		a.Title, a.Description, a.FilePath, a.Body, a.EmbedModel =
			deref(strs[0]), deref(strs[1]), deref(strs[2]), deref(strs[3]), deref(strs[4])
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &a.Meta)
		}
		a.DeletedAt = deleted
		out = append(out, ScoredArtifact{Artifact: &a, Distance: dist})
	}
	return out, rows.Err()
}

// ArtifactsMissingEmbedding returns up to n artifacts with no embedding.
func (s *Store) ArtifactsMissingEmbedding(ctx context.Context, n int) ([]*anamnesia.Artifact, error) {
	return s.QueryArtifacts(ctx, fmt.Sprintf(`SELECT %s FROM artifacts
		WHERE deleted_at IS NULL AND embedding IS NULL
		ORDER BY occurred_at ASC LIMIT $1`, artifactCols), []any{n})
}

// SetArtifactEmbedding stores a computed vector.
func (s *Store) SetArtifactEmbedding(ctx context.Context, id uuid.UUID, vec []float32, model string) error {
	v := pgvector.NewVector(vec)
	_, err := s.Pool.Exec(ctx,
		`UPDATE artifacts SET embedding = $2, embed_model = $3 WHERE id = $1`, id, v, model)
	return err
}

// ForgetArtifact soft-deletes one, for an artifact deleted on claude.ai.
func (s *Store) ForgetArtifact(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE artifacts SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// scanArtifactWithProject scans the listing query, which carries the
// project slug as a trailing column.
func scanArtifactWithProject(row rowScanner) (*anamnesia.Artifact, error) {
	var (
		a       anamnesia.Artifact
		project *uuid.UUID
		strs    [5]*string
		meta    []byte
		deleted *time.Time
		slug    string
	)
	err := row.Scan(
		&a.ID, &a.Scope.UserID, &project, &a.ArtifactUUID, &a.URL, &strs[0], &strs[1],
		&strs[2], &strs[3], &meta, &strs[4],
		&a.OccurredAt, &a.CreatedAt, &a.UpdatedAt, &deleted, &slug,
	)
	if err != nil {
		return nil, err
	}
	a.Scope.ProjectID = project
	a.Title, a.Description, a.FilePath, a.Body, a.EmbedModel =
		deref(strs[0]), deref(strs[1]), deref(strs[2]), deref(strs[3]), deref(strs[4])
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &a.Meta)
	}
	a.DeletedAt = deleted
	a.Project = slug
	return &a, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func scanArtifact(row rowScanner) (*anamnesia.Artifact, error) {
	var (
		a              anamnesia.Artifact
		project        *uuid.UUID
		title, desc    *string
		filePath, body *string
		embedModel     *string
		metaJSON       []byte
		deletedAt      *time.Time
	)
	err := row.Scan(
		&a.ID, &a.Scope.UserID, &project, &a.ArtifactUUID, &a.URL, &title, &desc,
		&filePath, &body, &metaJSON, &embedModel,
		&a.OccurredAt, &a.CreatedAt, &a.UpdatedAt, &deletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Scope.ProjectID = project
	if title != nil {
		a.Title = *title
	}
	if desc != nil {
		a.Description = *desc
	}
	if filePath != nil {
		a.FilePath = *filePath
	}
	if body != nil {
		a.Body = *body
	}
	if embedModel != nil {
		a.EmbedModel = *embedModel
	}
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &a.Meta)
	}
	a.DeletedAt = deletedAt
	return &a, nil
}
