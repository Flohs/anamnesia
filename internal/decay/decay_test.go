package decay

import (
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// TestHalfLifeGovernsRelevance is the test that makes the setting worth
// having: a configured half-life has to reach the SQL that recomputes
// relevance, or the config file is decorative.
func TestHalfLifeGovernsRelevance(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	uid, err := st.EnsureUser(ctx, "decay-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatal(err)
	}
	exp := &anamnesia.Experience{
		Scope:      anamnesia.Scope{UserID: uid},
		Kind:       anamnesia.ExperienceCase,
		Title:      "an episode",
		Body:       "body",
		Importance: 1,
	}
	if err := st.RecordExperience(ctx, exp); err != nil {
		t.Fatal(err)
	}
	// Exactly one 72 hour period ago, and never used since.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE experiences SET last_used_at = now() - interval '72 hours', use_count = 0
		  WHERE id = $1`, exp.ID); err != nil {
		t.Fatal(err)
	}

	relevance := func() float64 {
		t.Helper()
		var r float64
		if err := st.Pool.QueryRow(ctx,
			`SELECT relevance FROM experiences WHERE id = $1`, exp.ID).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	tick := func(halfLife time.Duration) {
		t.Helper()
		w := &Worker{
			Store: st,
			Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			Cfg: Config{HalfLives: map[anamnesia.ExperienceKind]time.Duration{
				anamnesia.ExperienceCase: halfLife,
			}},
		}
		if err := w.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}

	// One half-life old, so half its importance is left.
	tick(72 * time.Hour)
	if got := relevance(); math.Abs(got-0.5) > 0.01 {
		t.Errorf("relevance = %.4f after one configured half-life, want 0.5", got)
	}

	// The shipped default is two weeks, under which the same row has
	// barely faded. Same row, same age: only the setting changed.
	tick(336 * time.Hour)
	want := math.Exp(-math.Ln2 * 72 / 336)
	if got := relevance(); math.Abs(got-want) > 0.01 {
		t.Errorf("relevance = %.4f under a two week half-life, want %.4f", got, want)
	}
}
