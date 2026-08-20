package store

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

func TestRecordMentionIsIdempotent(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	uid, err := st.EnsureUser(ctx, "graph-mention-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), "graph-mention-test") })
	scope := anamnesia.Scope{UserID: uid}

	ent := &anamnesia.Entity{Scope: scope, Kind: "service", Name: "reconciliation-job"}
	if err := st.UpsertEntity(ctx, ent); err != nil {
		t.Fatal(err)
	}
	src := &anamnesia.Source{Scope: scope, Kind: "claude-session-graph", RawContent: "x"}
	if err := st.InsertSource(ctx, src); err != nil {
		t.Fatal(err)
	}

	// Twice: a re-extraction of the same source must not error.
	for i := 0; i < 2; i++ {
		if err := st.RecordMention(ctx, ent.ID, src.ID); err != nil {
			t.Fatalf("RecordMention call %d: %v", i+1, err)
		}
	}
	ents, err := st.EntitiesForSources(ctx, []uuid.UUID{src.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].ID != ent.ID {
		t.Errorf("EntitiesForSources = %v, want exactly the one entity", ents)
	}
}

func TestSourcesForEntitiesRoundTrips(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	uid, err := st.EnsureUser(ctx, "graph-roundtrip-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), "graph-roundtrip-test") })
	scope := anamnesia.Scope{UserID: uid}

	ent := &anamnesia.Entity{Scope: scope, Kind: "site", Name: "rotterdam"}
	if err := st.UpsertEntity(ctx, ent); err != nil {
		t.Fatal(err)
	}
	var srcIDs []uuid.UUID
	for i := 0; i < 2; i++ {
		src := &anamnesia.Source{Scope: scope, Kind: "claude-session-graph", RawContent: "x"}
		if err := st.InsertSource(ctx, src); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordMention(ctx, ent.ID, src.ID); err != nil {
			t.Fatal(err)
		}
		srcIDs = append(srcIDs, src.ID)
	}
	got, err := st.SourcesForEntities(ctx, []uuid.UUID{ent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("SourcesForEntities = %v, want both sources", got)
	}
}

func TestLookupEntitiesByName(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	uid, err := st.EnsureUser(ctx, "graph-lookup-by-name-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), "graph-lookup-by-name-test") })
	scope := anamnesia.Scope{UserID: uid}

	site := &anamnesia.Entity{Scope: scope, Kind: "site", Name: "rotterdam"}
	if err := st.UpsertEntity(ctx, site); err != nil {
		t.Fatal(err)
	}

	// One match: the name resolves unambiguously.
	matches, err := st.LookupEntitiesByName(ctx, scope, "rotterdam")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].ID != site.ID {
		t.Errorf("LookupEntitiesByName = %v, want exactly the one site entity", matches)
	}

	// A second entity with the same name under a different kind makes
	// the name ambiguous: both are reported, neither is picked for you.
	person := &anamnesia.Entity{Scope: scope, Kind: "person", Name: "rotterdam"}
	if err := st.UpsertEntity(ctx, person); err != nil {
		t.Fatal(err)
	}
	matches, err = st.LookupEntitiesByName(ctx, scope, "rotterdam")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Errorf("LookupEntitiesByName returned %d matches for an ambiguous name, want 2", len(matches))
	}
}

func TestMentionsVanishWithTheirEntity(t *testing.T) {
	// ON DELETE CASCADE, so a purged entity cannot leave dangling rows.
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	uid, err := st.EnsureUser(ctx, "graph-cascade-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), "graph-cascade-test") })
	scope := anamnesia.Scope{UserID: uid}

	ent := &anamnesia.Entity{Scope: scope, Kind: "service", Name: "doomed"}
	if err := st.UpsertEntity(ctx, ent); err != nil {
		t.Fatal(err)
	}
	src := &anamnesia.Source{Scope: scope, Kind: "claude-session-graph", RawContent: "x"}
	if err := st.InsertSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMention(ctx, ent.ID, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM entities WHERE id = $1`, ent.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM entity_mentions WHERE entity_id = $1`, ent.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d mentions survived their entity; the cascade did not fire", n)
	}
}

func TestNearestEntitiesRanksByDistance(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	uid, err := st.EnsureUser(ctx, "entity-nearest-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), "entity-nearest-test") })
	scope := anamnesia.Scope{UserID: uid}

	// Three entities at known distances from the probe, using synthetic
	// vectors so the assertion does not depend on an embedding model.
	dims := 1536
	mk := func(name string, first float32) *anamnesia.Entity {
		v := make([]float32, dims)
		v[0] = first
		v[1] = 1 - first
		e := &anamnesia.Entity{Scope: scope, Kind: "person", Name: name, Embedding: v}
		if err := st.UpsertEntity(ctx, e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	near := mk("priya-raman", 1.0)
	mid := mk("priha-raman", 0.9)
	far := mk("dana-okafor", 0.0)

	probe := make([]float32, dims)
	probe[0] = 1.0
	got, err := st.NearestEntities(ctx, scope, probe, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d matches, want 3", len(got))
	}
	if got[0].Entity.ID != near.ID || got[1].Entity.ID != mid.ID || got[2].Entity.ID != far.ID {
		t.Errorf("order = %s, %s, %s; want nearest first",
			got[0].Entity.Name, got[1].Entity.Name, got[2].Entity.Name)
	}
	if !(got[0].Distance < got[1].Distance && got[1].Distance < got[2].Distance) {
		t.Errorf("distances not ascending: %v, %v, %v",
			got[0].Distance, got[1].Distance, got[2].Distance)
	}
	_ = far
}

func TestNearestEntitiesSkipsEntitiesWithoutAnEmbedding(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	uid, err := st.EnsureUser(ctx, "entity-nullvec-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), "entity-nullvec-test") })
	scope := anamnesia.Scope{UserID: uid}

	// Every entity created before this feature has a NULL embedding. They
	// must be invisible to the query rather than sorting arbitrarily.
	if err := st.UpsertEntity(ctx, &anamnesia.Entity{Scope: scope, Kind: "person", Name: "no-vector"}); err != nil {
		t.Fatal(err)
	}
	probe := make([]float32, 1536)
	probe[0] = 1
	got, err := st.NearestEntities(ctx, scope, probe, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d matches, want 0: an entity with no embedding must not match", len(got))
	}
}

func TestNearestEntitiesStaysInsideItsScope(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	mine, err := st.EnsureUser(ctx, "entity-scope-mine")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := st.EnsureUser(ctx, "entity-scope-theirs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.DeleteUser(context.Background(), "entity-scope-mine")
		_, _ = st.DeleteUser(context.Background(), "entity-scope-theirs")
	})

	v := make([]float32, 1536)
	v[0] = 1
	if err := st.UpsertEntity(ctx, &anamnesia.Entity{
		Scope: anamnesia.Scope{UserID: theirs}, Kind: "person", Name: "someone-elses", Embedding: v,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.NearestEntities(ctx, anamnesia.Scope{UserID: mine}, v, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d matches from another user's scope, want 0", len(got))
	}
}
