package mcp

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// nameEmbedder gives every text a distinct unit vector, so a test can
// tell an entity that got a vector from one that did not without
// depending on which vector it got.
type nameEmbedder struct{}

func (nameEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, 1536)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}
func (nameEmbedder) Dims() int     { return 1536 }
func (nameEmbedder) Model() string { return "name-test" }

func callGraphEntity(t *testing.T, d Deps, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	res, err := d.graphEntity(context.Background(), req)
	if err != nil {
		t.Fatalf("graphEntity: %v", err)
	}
	if res.IsError {
		t.Fatalf("graphEntity returned an error result: %+v", res.Content)
	}
	return res
}

// anamnesia_graph_entity is the second writer to `entities`, and it has
// to agree with the graph extractor on how an entity is identified and
// on carrying a name vector. Written raw and unembedded, an entity
// created here is a node the extractor can never merge with (wrong
// normalisation, and no vector for candidate recall) and that
// graphExpand can never reach.
func TestGraphEntityToolNormalisesAndEmbedsTheName(t *testing.T) {
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
	user := "mcp-graph-entity-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, user)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUser(context.Background(), user) })
	scope := anamnesia.Scope{UserID: uid}

	d := Deps{Store: st, Embedder: nameEmbedder{}, DefaultUser: user}
	callGraphEntity(t, d, map[string]any{
		"kind": "Site",
		"name": "The Rotterdam Warehouse",
	})

	// The extractor would have written exactly this pair for the same
	// text, so a later checkpoint saying "rotterdam warehouse" lands on
	// this node instead of forking beside it.
	ent, err := st.LookupEntity(ctx, scope, "site", "rotterdam-warehouse")
	if err != nil {
		t.Fatalf("lookup (site, rotterdam-warehouse): %v — the tool wrote a name or kind the extractor would never look up", err)
	}

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
	t.Fatalf("the entity the tool created is invisible to NearestEntities (%d matches): it can never be a merge candidate", len(got))
}
