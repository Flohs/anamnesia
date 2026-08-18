package store

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// testStore opens the database named by ANAMNESIA_TEST_DATABASE_URL and
// returns a store plus a scope nobody else is writing to. Skips when
// there is no database, so the offline suite stays green.
func testStore(t *testing.T) (*Store, anamnesia.Scope) {
	t.Helper()
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	uid, err := st.EnsureUser(ctx, "browse-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	pid, err := st.EnsureProject(ctx, uid, "browse-project")
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	return st, anamnesia.Scope{UserID: uid, ProjectID: &pid}
}

func TestStatsCountsWhatIsInScope(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeProject,
		Key: "deploy.target", Value: map[string]any{"v": "fly.io"}, Trust: 0.8,
	}); err != nil {
		t.Fatal(err)
	}
	for _, abstraction := range []int{0, 0, 1} {
		if err := st.RecordExperience(ctx, &anamnesia.Experience{
			Scope: scope, Kind: anamnesia.ExperienceCase,
			Title: "an experience", Body: "body", Abstraction: abstraction,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.InsertSource(ctx, &anamnesia.Source{
		Scope: scope, Kind: "chat-turn", RawContent: "some content",
	}); err != nil {
		t.Fatal(err)
	}

	stats, err := st.Stats(ctx, scope)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Facts != 1 {
		t.Errorf("facts = %d, want 1", stats.Facts)
	}
	if stats.Experiences != 3 {
		t.Errorf("experiences = %d, want 3", stats.Experiences)
	}
	if stats.Sources != 1 {
		t.Errorf("sources = %d, want 1", stats.Sources)
	}
	if stats.Users < 1 || stats.Projects < 1 {
		t.Errorf("users = %d, projects = %d; both are server-wide and cannot be zero here",
			stats.Users, stats.Projects)
	}
	if got := stats.SourcesByState["pending"]; got != 1 {
		t.Errorf("sources pending = %d, want 1", got)
	}
	if got := stats.ExperiencesByAbstraction[0]; got != 2 {
		t.Errorf("abstraction 0 = %d, want 2", got)
	}
	if got := stats.ExperiencesByAbstraction[1]; got != 1 {
		t.Errorf("abstraction 1 = %d, want 1", got)
	}
	// Nothing is embedded: there is no embedder in this test, and the UI
	// leans on this to explain why vector search finds nothing.
	cov := stats.EmbeddingCoverage["experiences"]
	if cov.Total != 3 || cov.Embedded != 0 {
		t.Errorf("experience coverage = %+v, want 3 total and 0 embedded", cov)
	}
}

func TestQueuePendingAllIsServerWide(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	if err := st.InsertSource(ctx, &anamnesia.Source{
		Scope: scope, Kind: "chat-turn", RawContent: "waiting for the extractor",
	}); err != nil {
		t.Fatal(err)
	}

	extract, embed, err := st.QueuePendingAll(ctx)
	if err != nil {
		t.Fatalf("queue pending: %v", err)
	}
	if extract < 1 {
		t.Errorf("extract pending = %d, want at least the source just written", extract)
	}
	if embed < 0 {
		t.Errorf("embed pending = %d", embed)
	}
}

func TestLookupUserDoesNotCreateOne(t *testing.T) {
	// The read API resolves scope with these. EnsureUser would turn a
	// typo in a query string into a row, which is exactly what a
	// read-only surface must not do.
	st, scope := testStore(t)
	ctx := context.Background()

	before, err := st.Stats(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.LookupUser(ctx, "no-such-user-"+uuid.NewString()[:8]); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("an unknown handle was reported as found")
	}
	after, err := st.Stats(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if after.Users != before.Users {
		t.Errorf("users went from %d to %d: the lookup created one", before.Users, after.Users)
	}
}

func TestLookupUserAndProjectFindWhatExists(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	handle, err := st.LookupUserHandle(ctx, scope.UserID)
	if err != nil {
		t.Fatal(err)
	}
	uid, ok, err := st.LookupUser(ctx, handle)
	if err != nil || !ok {
		t.Fatalf("lookup user: ok=%v err=%v", ok, err)
	}
	if uid != scope.UserID {
		t.Errorf("user id = %s, want %s", uid, scope.UserID)
	}
	pid, ok, err := st.LookupProject(ctx, uid, "browse-project")
	if err != nil || !ok {
		t.Fatalf("lookup project: ok=%v err=%v", ok, err)
	}
	if pid != *scope.ProjectID {
		t.Errorf("project id = %s, want %s", pid, *scope.ProjectID)
	}
	if _, ok, err := st.LookupProject(ctx, uid, "no-such-project"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("an unknown slug was reported as found")
	}
}

func TestListProjectsCarriesCountsAndLastActivity(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeProject,
		Key: "deploy.target", Value: map[string]any{"v": "fly.io"}, Trust: 0.8,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordExperience(ctx, &anamnesia.Experience{
		Scope: scope, Kind: anamnesia.ExperienceCase, Title: "did a thing", Body: "body",
	}); err != nil {
		t.Fatal(err)
	}

	projects, err := st.ListProjects(ctx, &scope.UserID)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects = %d, want the one this user has", len(projects))
	}
	p := projects[0]
	if p.Slug != "browse-project" || p.ID != *scope.ProjectID {
		t.Errorf("project = %+v", p)
	}
	if p.Counts.Facts != 1 || p.Counts.Experiences != 1 {
		t.Errorf("counts = %+v, want one fact and one experience", p.Counts)
	}
	if p.LastActivity == nil {
		t.Error("last activity is nil after writing two rows")
	}
	if p.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}
}

func TestListUsersCarriesCounts(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	if err := st.RecordExperience(ctx, &anamnesia.Experience{
		Scope: scope, Kind: anamnesia.ExperienceCase, Title: "did a thing", Body: "body",
	}); err != nil {
		t.Fatal(err)
	}
	handle, err := st.LookupUserHandle(ctx, scope.UserID)
	if err != nil {
		t.Fatal(err)
	}

	users, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	var mine *UserSummary
	for i := range users {
		if users[i].Handle == handle {
			mine = &users[i]
		}
	}
	if mine == nil {
		t.Fatalf("user %s missing from %d users", handle, len(users))
	}
	if mine.Projects != 1 {
		t.Errorf("projects = %d, want 1", mine.Projects)
	}
	if mine.Counts.Experiences != 1 {
		t.Errorf("counts = %+v, want one experience", mine.Counts)
	}
}

func TestDetailGettersFindOneRow(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	sk := &anamnesia.Skill{Scope: scope, Name: "run-tests", Kind: "command", Description: "runs the suite"}
	if err := st.RegisterSkill(ctx, sk); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSkill(ctx, sk.ID)
	if err != nil || got.Name != "run-tests" {
		t.Fatalf("get skill: %+v %v", got, err)
	}

	from := &anamnesia.Entity{Scope: scope, Kind: "person", Name: "ada"}
	to := &anamnesia.Entity{Scope: scope, Kind: "project", Name: "anamnesia"}
	if err := st.UpsertEntity(ctx, from); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEntity(ctx, to); err != nil {
		t.Fatal(err)
	}
	edge := &anamnesia.Edge{From: from.ID, To: to.ID, Kind: "works_on"}
	if err := st.CreateEdge(ctx, edge); err != nil {
		t.Fatal(err)
	}
	gotEdge, err := st.GetEdge(ctx, edge.ID)
	if err != nil || gotEdge.Kind != "works_on" {
		t.Fatalf("get edge: %+v %v", gotEdge, err)
	}

	session := uuid.New()
	wm := &anamnesia.WorkingEntry{
		Scope: scope, SessionID: session, Position: 1, Role: "user", Body: "a note",
	}
	if err := st.AppendWorking(ctx, wm); err != nil {
		t.Fatal(err)
	}
	gotWM, err := st.GetWorking(ctx, wm.ID)
	if err != nil || gotWM.Body != "a note" {
		t.Fatalf("get working: %+v %v", gotWM, err)
	}

	if _, err := st.GetSkill(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown skill error = %v, want ErrNotFound", err)
	}
	if _, err := st.GetEdge(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown edge error = %v, want ErrNotFound", err)
	}
}

func TestActivityBucketsCountPerDayAndProject(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	if err := st.RecordExperience(ctx, &anamnesia.Experience{
		Scope: scope, Kind: anamnesia.ExperienceCase, Title: "t", Body: "b",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertSource(ctx, &anamnesia.Source{
		Scope: scope, Kind: "chat-turn", RawContent: "content",
	}); err != nil {
		t.Fatal(err)
	}

	buckets, err := st.ActivityBuckets(ctx, scope, 30)
	if err != nil {
		t.Fatalf("buckets: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets = %+v, want one day", buckets)
	}
	b := buckets[0]
	if b.Project == nil || *b.Project != "browse-project" {
		t.Errorf("project = %v, want browse-project", b.Project)
	}
	if b.Experiences != 1 || b.Sources != 1 {
		t.Errorf("bucket = %+v, want one experience and one source", b)
	}
	if b.Date == "" {
		t.Error("bucket has no date")
	}
}

func TestEmbeddingSampleReturnsVectors(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()
	exp := &anamnesia.Experience{
		Scope: scope, Kind: anamnesia.ExperienceCase, Title: "has a vector", Body: "b",
	}
	if err := st.RecordExperience(ctx, exp); err != nil {
		t.Fatal(err)
	}
	vec := make([]float32, 1536)
	for i := range vec {
		vec[i] = float32(i%7) / 7
	}
	if err := st.SetExperienceEmbedding(ctx, exp.ID, vec, "test-model"); err != nil {
		t.Fatal(err)
	}
	// A row without an embedding must not appear: it has no position.
	if err := st.RecordExperience(ctx, &anamnesia.Experience{
		Scope: scope, Kind: anamnesia.ExperienceCase, Title: "no vector", Body: "b",
	}); err != nil {
		t.Fatal(err)
	}

	sample, err := st.EmbeddingSample(ctx, "experiences", scope, 100)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if len(sample) != 1 {
		t.Fatalf("sample = %d rows, want only the embedded one", len(sample))
	}
	if sample[0].Title != "has a vector" || sample[0].Kind != "case" {
		t.Errorf("row = %+v", sample[0])
	}
	if len(sample[0].Vector) != 1536 {
		t.Fatalf("vector width = %d, want 1536", len(sample[0].Vector))
	}
	if sample[0].Vector[1] == 0 && sample[0].Vector[2] == 0 {
		t.Error("the vector decoded as zeroes")
	}
	if sample[0].Project == nil || *sample[0].Project != "browse-project" {
		t.Errorf("project = %v", sample[0].Project)
	}
}
