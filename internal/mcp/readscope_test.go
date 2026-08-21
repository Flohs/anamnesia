package mcp

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
)

func mcpScopeStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	handle := "mcpscope-" + uuid.NewString()[:8]
	if _, err := st.EnsureUser(ctx, handle); err != nil {
		t.Fatal(err)
	}
	return st, handle
}

func mcpProjectExists(t *testing.T, st *store.Store, handle, slug string) bool {
	t.Helper()
	uid, _, err := st.LookupUser(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	_, found, err := st.LookupProject(context.Background(), uid, slug)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestAnMCPReadDoesNotCreateAProject: the MCP server carries its own copy
// of resolveScope, with the same EnsureProject in it, so a read-only tool
// like anamnesia_search created the project it was asked about.
func TestAnMCPReadDoesNotCreateAProject(t *testing.T) {
	st, handle := mcpScopeStore(t)
	d := Deps{Store: st, DefaultUser: handle}
	slug := "mcp-never-" + uuid.NewString()[:8]

	scope, err := d.resolveScope(context.Background(), map[string]any{"user": handle, "project": slug})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if mcpProjectExists(t, st, handle, slug) {
		t.Errorf("a read created the project %q", slug)
	}
	if scope.ProjectID == nil {
		t.Fatal("an unknown project resolved to a nil id, which drops the project filter and reads every project")
	}
	if *scope.ProjectID != uuid.Nil {
		t.Errorf("ProjectID = %v, want the nil uuid so it matches no project row", *scope.ProjectID)
	}
}

// TestAnMCPWriteCreatesTheProject: every project-scoped table has a
// foreign key to projects, so a write that skipped this would fail on the
// constraint rather than land.
func TestAnMCPWriteCreatesTheProject(t *testing.T) {
	st, handle := mcpScopeStore(t)
	d := Deps{Store: st, DefaultUser: handle}
	slug := "mcp-written-" + uuid.NewString()[:8]

	scope, err := d.resolveWriteScope(context.Background(), map[string]any{"user": handle, "project": slug})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if scope.ProjectID == nil || *scope.ProjectID == uuid.Nil {
		t.Fatalf("write scope has no usable project id: %v", scope.ProjectID)
	}
	if !mcpProjectExists(t, st, handle, slug) {
		t.Errorf("a write did not create the project %q", slug)
	}
}
