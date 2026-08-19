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
