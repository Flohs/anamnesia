package jobs

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// fixedEmbedder hands back the same vector for every text, at the width
// the test database's schema was migrated to. Enough for a backfill
// test: what matters is that a row went from NULL to a vector, not which
// vector it got.
//
// FactsMissingEmbedding and ExperiencesMissingEmbedding are server-wide,
// not scoped to this test's user, so a tick here also embeds any row
// another package's test left unembedded at the same moment. Harmless —
// they get a real, well-formed vector — but worth knowing when reading a
// surprising failure elsewhere.
type fixedEmbedder struct{}

func (fixedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, 1536)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}
func (fixedEmbedder) Dims() int     { return 1536 }
func (fixedEmbedder) Model() string { return "fixed-test" }

// newEmbedTestStore opens the test database and a fresh user scope,
// registering cleanup.
func newEmbedTestStore(t *testing.T, userPrefix string) (*store.Store, anamnesia.Scope) {
	t.Helper()
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	user := userPrefix + "-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, user)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), user) })
	return st, anamnesia.Scope{UserID: uid}
}

// maxEmbedTicks bounds drainEmbedQueue so a backfill that genuinely
// never happens fails the test instead of hanging it.
const maxEmbedTicks = 20

// drainEmbedQueue runs embed ticks until this scope's embed queue is
// empty, or maxEmbedTicks have passed. A loop rather than a single tick
// because FactsMissingEmbedding and friends are server-wide and
// oldest-first: on a long-lived test database this user's brand new row
// sits behind however many older unembedded rows other tests left
// behind, so one batch need not reach it. The real worker ticks on a
// timer for the same reason.
func drainEmbedQueue(t *testing.T, st *store.Store, scope anamnesia.Scope) {
	t.Helper()
	ctx := context.Background()
	w := &Worker{Cfg: Config{EmbedBatch: 32}, Store: st, Embedder: fixedEmbedder{}, Log: discardLog()}
	for i := 0; i < maxEmbedTicks; i++ {
		if _, err := w.tickEmbed(ctx); err != nil {
			t.Fatalf("tickEmbed: %v", err)
		}
		_, pending, err := st.QueuePending(ctx, scope.UserID)
		if err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			return
		}
	}
}

// An entity's vector is its NAME, and it is the only thing entity
// resolution recalls merge candidates by. An entity created while the
// embedder was down carries none — and so does every entity in the
// database after `migrate --dims N`, the documented repair for a
// width mismatch, which does ALTER COLUMN ... USING NULL. Without a
// backfill those entities are unmergeable forever, silently.
func TestEmbedTickBackfillsEntityNames(t *testing.T) {
	st, scope := newEmbedTestStore(t, "embed-entity-backfill")
	ctx := context.Background()

	ent := &anamnesia.Entity{Scope: scope, Kind: "person", Name: "priya-raman"}
	if err := st.UpsertEntity(ctx, ent); err != nil {
		t.Fatalf("upsert entity: %v", err)
	}

	drainEmbedQueue(t, st, scope)

	probe := make([]float32, 1536)
	probe[0] = 1
	got, err := st.NearestEntities(ctx, scope, probe, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if m.Entity.ID == ent.ID {
			return
		}
	}
	t.Fatalf("after an embed tick the entity is still invisible to NearestEntities (%d matches): it can never be offered as a merge candidate", len(got))
}

// QueuePending counts entities missing an embedding into embed_pending,
// so anything that waits for embed_pending to reach 0 — `anamnesia
// eval`'s drain, the documented benchmark pattern — hangs forever on an
// entity nothing ever backfills.
func TestEmbedPendingDrainsWithAnUnembeddedEntity(t *testing.T) {
	st, scope := newEmbedTestStore(t, "embed-entity-pending")
	ctx := context.Background()

	if err := st.UpsertEntity(ctx, &anamnesia.Entity{Scope: scope, Kind: "service", Name: "stock-reconciliation"}); err != nil {
		t.Fatalf("upsert entity: %v", err)
	}
	_, pending, err := st.QueuePending(ctx, scope.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if pending == 0 {
		t.Fatal("embed_pending is already 0 with an unembedded entity present; this test cannot observe the drain")
	}

	drainEmbedQueue(t, st, scope)

	_, pending, err = st.QueuePending(ctx, scope.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("embed_pending = %d after %d embed ticks, want 0: a drain waiting on this never returns", pending, maxEmbedTicks)
	}
}
