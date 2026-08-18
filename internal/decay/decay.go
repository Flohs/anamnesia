// Package decay implements the background eviction / forgetting policy.
//
// Three orthogonal signals combine into a single relevance score on the
// experiences table:
//
//	time:       Ebbinghaus exponential decay over LastUsedAt
//	frequency:  log(1 + use_count) / log(101)
//	importance: stored at ingest, never decayed
//
// Half-lives differ per ExperienceKind: case_based decays in weeks,
// strategy_based effectively forever. Facts and skills don't get a
// relevance score — facts have explicit valid_to, skills decay only on
// disuse via app code.
//
// Compaction sweeps soft-deleted rows older than the window from facts +
// experiences.
package decay

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// Config controls the worker.
type Config struct {
	Interval         time.Duration
	HalfLives        map[anamnesia.ExperienceKind]time.Duration
	CompactionWindow time.Duration
	LRUArchiveAfter  time.Duration
}

// Worker periodically updates relevance and compacts deleted rows.
type Worker struct {
	Store *store.Store
	Log   *slog.Logger
	Cfg   Config
}

// Tick runs one pass of the decay loop. The caller schedules the cadence.
func (w *Worker) Tick(ctx context.Context) error {
	c := w.applyDefaults()
	if err := w.recomputeRelevance(ctx, c); err != nil {
		return err
	}
	if _, err := w.compactFacts(ctx, c.CompactionWindow); err != nil {
		return err
	}
	if _, err := w.compactExperiences(ctx, c.CompactionWindow); err != nil {
		return err
	}
	return nil
}

// recomputeRelevance updates experiences.relevance per ExperienceKind.
func (w *Worker) recomputeRelevance(ctx context.Context, c Config) error {
	for kind, half := range c.HalfLives {
		halfHours := half.Hours()
		if halfHours <= 0 {
			continue
		}
		lambda := math.Ln2 / halfHours
		// Saturate the exponent at -80 so float4 doesn't underflow.
		_, err := w.Store.Pool.Exec(ctx, `
			UPDATE experiences
			   SET relevance = LEAST(1.0, GREATEST(0.0,
				   importance * exp(GREATEST(-80::float8, -$1::float8 * EXTRACT(EPOCH FROM (now() - last_used_at))::float8 / 3600.0))
				 + 0.05 * LEAST(1.0, ln(1 + use_count) / ln(101.0))
			   ))
			 WHERE kind = $2 AND deleted_at IS NULL`,
			lambda, string(kind))
		if err != nil {
			return err
		}
	}
	if c.LRUArchiveAfter > 0 {
		_, err := w.Store.Pool.Exec(ctx, `
			UPDATE experiences
			   SET relevance = relevance * 0.25
			 WHERE deleted_at IS NULL
			   AND last_used_at < now() - $1::interval`, c.LRUArchiveAfter.String())
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) compactFacts(ctx context.Context, window time.Duration) (int64, error) {
	tag, err := w.Store.Pool.Exec(ctx,
		`DELETE FROM facts WHERE deleted_at IS NOT NULL AND deleted_at < now() - $1::interval`,
		window.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (w *Worker) compactExperiences(ctx context.Context, window time.Duration) (int64, error) {
	tag, err := w.Store.Pool.Exec(ctx,
		`DELETE FROM experiences WHERE deleted_at IS NOT NULL AND deleted_at < now() - $1::interval`,
		window.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (w *Worker) applyDefaults() Config {
	c := w.Cfg
	if c.Interval == 0 {
		c.Interval = time.Hour
	}
	if c.CompactionWindow == 0 {
		c.CompactionWindow = 24 * time.Hour
	}
	if c.LRUArchiveAfter == 0 {
		c.LRUArchiveAfter = 90 * 24 * time.Hour
	}
	if c.HalfLives == nil {
		c.HalfLives = DefaultHalfLives()
	}
	return c
}

// DefaultHalfLives is the shipped forgetting policy: a case decays in
// weeks, a strategy effectively never, a hybrid somewhere between.
//
// Exported because these are also configuration defaults, declared in
// cmd/anamnesia/settings.go, and a test there checks the two agree. A
// default that lives in two places drifts.
func DefaultHalfLives() map[anamnesia.ExperienceKind]time.Duration {
	return map[anamnesia.ExperienceKind]time.Duration{
		anamnesia.ExperienceCase:     14 * 24 * time.Hour,
		anamnesia.ExperienceStrategy: 365 * 24 * time.Hour,
		anamnesia.ExperienceHybrid:   60 * 24 * time.Hour,
	}
}
