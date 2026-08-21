package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
)

func scopeStore(t *testing.T) (*store.Store, string) {
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
	handle := "scope-" + uuid.NewString()[:8]
	if _, err := st.EnsureUser(ctx, handle); err != nil {
		t.Fatal(err)
	}
	return st, handle
}

func projectExists(t *testing.T, st *store.Store, handle, slug string) bool {
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

// TestReadingDoesNotCreateAProject. resolveScope called EnsureProject,
// and eleven endpoints used it, most of them reads. SessionStart and
// UserPromptSubmit fire in every directory Claude Code is opened in, so
// simply opening a repository created a project row before anything was
// ever written to it. On one real install that left 10 projects holding
// no sources, no facts and no experiences.
func TestReadingDoesNotCreateAProject(t *testing.T) {
	st, handle := scopeStore(t)
	d := Deps{Store: st, DefaultUser: handle}
	slug := "never-written-" + uuid.NewString()[:8]

	if _, err := d.resolveScope(context.Background(), &HookEvent{User: handle, Project: slug}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if projectExists(t, st, handle, slug) {
		t.Errorf("reading created the project %q", slug)
	}
}

// TestAnUnknownProjectStillScopesTheSearch is the trap in the fix.
// retrieval.Search omits the project filter entirely when Scope.ProjectID
// is nil, which means "every project", not "no project". Returning a nil
// id for a project that does not exist would turn the first prompt in a
// brand new repository into a read across everything the user has ever
// stored, in every other project.
func TestAnUnknownProjectStillScopesTheSearch(t *testing.T) {
	st, handle := scopeStore(t)
	d := Deps{Store: st, DefaultUser: handle}
	slug := "never-written-" + uuid.NewString()[:8]

	scope, err := d.resolveScope(context.Background(), &HookEvent{User: handle, Project: slug})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if scope.ProjectID == nil {
		t.Fatal("an unknown project resolved to a nil id, which drops the project filter and reads every project")
	}
	if *scope.ProjectID != uuid.Nil {
		t.Errorf("ProjectID = %v, want the nil uuid: it must match no project row", *scope.ProjectID)
	}
}

// TestAKnownProjectStillResolvesToItself guards the over-correction.
func TestAKnownProjectStillResolvesToItself(t *testing.T) {
	st, handle := scopeStore(t)
	ctx := context.Background()
	uid, _, err := st.LookupUser(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	slug := "real-" + uuid.NewString()[:8]
	pid, err := st.EnsureProject(ctx, uid, slug)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Store: st, DefaultUser: handle}

	scope, err := d.resolveScope(ctx, &HookEvent{User: handle, Project: slug})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if scope.ProjectID == nil || *scope.ProjectID != pid {
		t.Errorf("ProjectID = %v, want %v", scope.ProjectID, pid)
	}
}

// TestWritingCreatesTheProject: a write has to have somewhere to land,
// and every project-scoped table carries a foreign key to projects, so a
// write against a project that was never created fails on the constraint.
func TestWritingCreatesTheProject(t *testing.T) {
	st, handle := scopeStore(t)
	d := Deps{Store: st, DefaultUser: handle}
	slug := "written-" + uuid.NewString()[:8]

	scope, err := d.resolveWriteScope(context.Background(), &HookEvent{User: handle, Project: slug})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if scope.ProjectID == nil || *scope.ProjectID == uuid.Nil {
		t.Fatalf("write scope has no usable project id: %v", scope.ProjectID)
	}
	if !projectExists(t, st, handle, slug) {
		t.Errorf("writing did not create the project %q", slug)
	}
}

// TestIngestOverHTTPCreatesTheProject pins the wiring, not just the
// helper: /v1/ingest must still be on the creating path.
func TestIngestOverHTTPCreatesTheProject(t *testing.T) {
	st, handle := scopeStore(t)
	srv := testServer(t, Deps{Store: st, DefaultUser: handle})
	slug := "ingested-" + uuid.NewString()[:8]

	body := `{"user":"` + handle + `","project":"` + slug + `","kind":"conversation","content":"hello"}`
	resp, err := http.Post(srv.URL+"/v1/ingest", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("ingest returned %d", resp.StatusCode)
	}
	if !projectExists(t, st, handle, slug) {
		t.Errorf("ingesting did not create the project %q", slug)
	}
}
