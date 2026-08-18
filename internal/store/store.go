// Package store is the Postgres-backed persistence layer for Anamnesia.
// Single-tenant edition: no RLS, no tenant_id columns. The whole DB is
// one trust boundary.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	stdlibadapter "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store owns the pgx pool. Safe for concurrent use.
type Store struct {
	Pool *pgxpool.Pool
}

// Open dials Postgres, verifies the connection, and returns a Store.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("dial postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// Migrate applies the embedded SQL migrations using goose.
func (s *Store) Migrate(ctx context.Context) error {
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	db := stdlibadapter.OpenDBFromPool(s.Pool)
	defer db.Close()
	return goose.UpContext(ctx, db, "migrations")
}

// Tx runs fn inside a transaction.
func (s *Store) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("anamnesia: not found")

// ─── User and project resolution ───────────────────────────────────────

// EnsureUser returns the user id for handle, creating the row if absent.
// Idempotent.
func (s *Store) EnsureUser(ctx context.Context, handle string) (uuid.UUID, error) {
	if handle == "" {
		return uuid.Nil, errors.New("user handle required")
	}
	var id uuid.UUID
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id FROM users WHERE handle = $1`, handle).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.QueryRow(ctx,
				`INSERT INTO users (handle) VALUES ($1) RETURNING id`,
				handle,
			).Scan(&id)
		}
		return err
	})
	return id, err
}

// EnsureProject returns the project id for (userID, slug), creating if absent.
func (s *Store) EnsureProject(ctx context.Context, userID uuid.UUID, slug string) (uuid.UUID, error) {
	if slug == "" {
		return uuid.Nil, errors.New("project slug required")
	}
	var id uuid.UUID
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT id FROM projects WHERE user_id = $1 AND slug = $2`,
			userID, slug,
		).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.QueryRow(ctx,
				`INSERT INTO projects (user_id, slug) VALUES ($1, $2) RETURNING id`,
				userID, slug,
			).Scan(&id)
		}
		return err
	})
	return id, err
}

// LookupUserHandle returns the handle for a user id.
// LookupUser resolves a handle without creating it. The read API needs
// this: EnsureUser would turn ?user=typo into a row, and an endpoint
// that promises to change nothing has to mean it.
func (s *Store) LookupUser(ctx context.Context, handle string) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := s.Pool.QueryRow(ctx, `SELECT id FROM users WHERE handle = $1`, handle).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, true, nil
}

// LookupProject resolves a slug within a user without creating it.
func (s *Store) LookupProject(ctx context.Context, userID uuid.UUID, slug string) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := s.Pool.QueryRow(ctx,
		`SELECT id FROM projects WHERE user_id = $1 AND slug = $2`, userID, slug).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, true, nil
}

func (s *Store) LookupUserHandle(ctx context.Context, id uuid.UUID) (string, error) {
	var h string
	err := s.Pool.QueryRow(ctx, `SELECT handle FROM users WHERE id = $1`, id).Scan(&h)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return h, err
}

// DeleteUser removes a user and, by cascade, every row that belongs to
// it. Only `anamnesia eval` calls this, to clean up the scope it created;
// nothing on the memory path deletes a user.
func (s *Store) DeleteUser(ctx context.Context, handle string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM users WHERE handle = $1`, handle)
	if err != nil {
		return false, fmt.Errorf("delete user %q: %w", handle, err)
	}
	return tag.RowsAffected() > 0, nil
}

// LookupProjectSlug returns the slug for a project id.
func (s *Store) LookupProjectSlug(ctx context.Context, id uuid.UUID) (string, error) {
	var s2 string
	err := s.Pool.QueryRow(ctx, `SELECT slug FROM projects WHERE id = $1`, id).Scan(&s2)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return s2, err
}
