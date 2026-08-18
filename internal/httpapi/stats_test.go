package httpapi

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// dbServer wires a server onto the test database with a scope of its
// own, and returns the handle and slug that address it.
func dbServer(t *testing.T, rec *activity.Recorder) (*store.Store, anamnesia.Scope, string, string, string) {
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
	handle := "api-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	slug := "api-project"
	pid, err := st.EnsureProject(ctx, uid, slug)
	if err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, Deps{
		Store: st, Activity: rec, DefaultUser: handle, DefaultProject: slug,
	})
	return st, anamnesia.Scope{UserID: uid, ProjectID: &pid}, handle, slug, srv.URL
}

func TestStatsReportsTotalsForTheScope(t *testing.T) {
	st, scope, handle, slug, base := dbServer(t, nil)
	ctx := context.Background()
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeProject,
		Key: "deploy.target", Value: map[string]any{"v": "fly.io"}, Trust: 0.8,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertSource(ctx, &anamnesia.Source{
		Scope: scope, Kind: "chat-turn", RawContent: "content",
	}); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if code := getJSON(t, base+"/v1/stats", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	scopeView := got["scope"].(map[string]any)
	if scopeView["user"] != handle || scopeView["project"] != slug {
		t.Errorf("scope = %v, want %s/%s", scopeView, handle, slug)
	}
	totals := got["totals"].(map[string]any)
	if totals["facts"] != float64(1) || totals["sources"] != float64(1) {
		t.Errorf("totals = %v, want one fact and one source", totals)
	}
	byState := got["sources_by_state"].(map[string]any)
	if byState["pending"] != float64(1) || byState["failed"] != float64(0) {
		t.Errorf("sources_by_state = %v, want pending 1 and failed reported as 0", byState)
	}
	if _, ok := got["queues"]; !ok {
		t.Error("stats carries no queues")
	}
	coverage := got["embedding_coverage"].(map[string]any)
	facts := coverage["facts"].(map[string]any)
	if facts["total"] != float64(1) || facts["embedded"] != float64(0) {
		t.Errorf("fact coverage = %v, want 1 total and 0 embedded", facts)
	}
}

func TestStatsRejectsAnUnknownScopeWithoutCreatingIt(t *testing.T) {
	_, _, _, _, base := dbServer(t, nil)
	if code := getJSON(t, base+"/v1/stats?user=definitely-not-a-user", nil); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 rather than a freshly created user", code)
	}
}

func TestActivityCarriesQueueCountsWhenThereIsAStore(t *testing.T) {
	rec := activity.New(4)
	st, scope, _, _, base := dbServer(t, rec)
	if err := st.InsertSource(context.Background(), &anamnesia.Source{
		Scope: scope, Kind: "chat-turn", RawContent: "waiting",
	}); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if code := getJSON(t, base+"/v1/activity", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	queues, ok := got["queues"].(map[string]any)
	if !ok {
		t.Fatalf("activity carries no queues: %v", got)
	}
	if queues["extract_pending"].(float64) < 1 {
		t.Errorf("extract_pending = %v, want at least the source just written", queues["extract_pending"])
	}
}

func TestProjectsListsCountsPerProject(t *testing.T) {
	st, scope, handle, slug, base := dbServer(t, nil)
	if err := st.RecordExperience(context.Background(), &anamnesia.Experience{
		Scope: scope, Kind: anamnesia.ExperienceCase, Title: "did a thing", Body: "body",
	}); err != nil {
		t.Fatal(err)
	}

	var got struct {
		Items []map[string]any `json:"items"`
	}
	if code := getJSON(t, base+"/v1/projects?user="+handle, &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %v, want the one project this user has", got.Items)
	}
	p := got.Items[0]
	if p["slug"] != slug || p["user"] != handle {
		t.Errorf("project = %v", p)
	}
	if p["last_activity"] == nil {
		t.Error("last_activity is null after writing an experience")
	}
	counts := p["counts"].(map[string]any)
	if counts["experiences"] != float64(1) {
		t.Errorf("counts = %v, want one experience", counts)
	}
}

func TestUsersListsEveryUser(t *testing.T) {
	_, _, handle, _, base := dbServer(t, nil)
	var got struct {
		Items []map[string]any `json:"items"`
	}
	if code := getJSON(t, base+"/v1/users", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	for _, u := range got.Items {
		if u["handle"] == handle {
			if u["projects"] != float64(1) {
				t.Errorf("projects = %v, want 1", u["projects"])
			}
			return
		}
	}
	t.Errorf("user %s missing from %d users", handle, len(got.Items))
}
