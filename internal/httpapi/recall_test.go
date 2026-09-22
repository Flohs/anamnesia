package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/embed"
	"github.com/flohs/anamnesia/internal/retrieval"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

func dist(v float64) *float64 { return &v }

func hitAt(d *float64) anamnesia.SearchHit {
	return anamnesia.SearchHit{Domain: anamnesia.DomainFact, Distance: d}
}

func TestGradeRecallCountsAHitInsideTheDistanceBar(t *testing.T) {
	hits := []anamnesia.SearchHit{hitAt(dist(0.9)), hitAt(dist(0.41))}
	if got := gradeRecall(hits, recallBars{MinScore: 0.5, MaxDistance: 0.6, TrustDistance: true}); got != store.RecallHit {
		t.Errorf("got %q, want %q: 0.41 is inside a 0.60 bar", got, store.RecallHit)
	}
}

func TestGradeRecallCountsAMissWhenEveryHitIsOutsideTheBar(t *testing.T) {
	hits := []anamnesia.SearchHit{hitAt(dist(0.71)), hitAt(dist(0.95))}
	if got := gradeRecall(hits, recallBars{MinScore: 0.5, MaxDistance: 0.6, TrustDistance: true}); got != store.RecallMiss {
		t.Errorf("got %q, want %q: nothing is inside a 0.60 bar", got, store.RecallMiss)
	}
}

// The reranker's score is absolute relevance on its own scale, so where
// one ran it is the better judge and the distance is beside the point.
func TestGradeRecallPrefersTheRerankerScoreWhenOneRan(t *testing.T) {
	far := hitAt(dist(0.95))
	far.RerankerRank = 1
	far.Score = 0.82
	if got := gradeRecall([]anamnesia.SearchHit{far}, recallBars{MinScore: 0.5, MaxDistance: 0.6, TrustDistance: true}); got != store.RecallHit {
		t.Errorf("got %q, want %q: a reranker score of 0.82 clears a 0.50 bar", got, store.RecallHit)
	}

	near := hitAt(dist(0.05))
	near.RerankerRank = 1
	near.Score = 0.2
	if got := gradeRecall([]anamnesia.SearchHit{near}, recallBars{MinScore: 0.5, MaxDistance: 0.6, TrustDistance: true}); got != store.RecallMiss {
		t.Errorf("got %q, want %q: the reranker judged it at 0.20", got, store.RecallMiss)
	}
}

// Lexical and graph hits carry no absolute number. Calling that a miss
// would blame retrieval for a question nobody can answer.
func TestGradeRecallIsUngradedWhenNoHitCarriesAnAbsoluteNumber(t *testing.T) {
	hits := []anamnesia.SearchHit{hitAt(nil), hitAt(nil)}
	if got := gradeRecall(hits, recallBars{MinScore: 0.5, MaxDistance: 0.6, TrustDistance: true}); got != store.RecallUngraded {
		t.Errorf("got %q, want %q", got, store.RecallUngraded)
	}
}

// Nothing came back at all, which is knowable without any bar.
func TestGradeRecallCountsNoHitsAsAMiss(t *testing.T) {
	if got := gradeRecall(nil, recallBars{MinScore: 0.5, MaxDistance: 0.6, TrustDistance: true}); got != store.RecallMiss {
		t.Errorf("got %q, want %q", got, store.RecallMiss)
	}
}

// ── the handler ──────────────────────────────────────────────────────

// keywordEmbedder puts each keyword on its own axis, so a text matching
// the query is at distance 0 and one matching nothing it mentions is at
// distance 1. That makes the bar assertable without a real model.
type keywordEmbedder struct{ keywords []string }

func (k keywordEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 1536)
		low := strings.ToLower(t)
		for j, w := range k.keywords {
			if strings.Contains(low, w) {
				v[j] = 1
			}
		}
		v[len(v)-1] = 0.0001 // no vector is all-zero; cosine has no answer for that
		out[i] = v
	}
	return out, nil
}
func (k keywordEmbedder) Dims() int     { return 1536 }
func (k keywordEmbedder) Model() string { return "keyword-test-embedder" }

// recallFixture is a user holding one embedded fact about quokkas.
func recallFixture(t *testing.T) (*store.Store, string, uuid.UUID, keywordEmbedder) {
	t.Helper()
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	handle := "recall-" + uuid.NewString()[:8]
	uid, err := st.EnsureUser(ctx, handle)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	return st, handle, uid, keywordEmbedder{keywords: []string{"quokka", "wombat"}}
}

func embeddedFact(t *testing.T, st *store.Store, emb keywordEmbedder, uid uuid.UUID, key, body string) {
	t.Helper()
	ctx := context.Background()
	v, err := emb.Embed(ctx, []string{key + " " + body})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: anamnesia.Scope{UserID: uid}, FactKind: anamnesia.FactScopeUser,
		Key: key, Value: map[string]any{"v": body}, Trust: 0.9,
		Embedding: v[0], EmbedModel: emb.Model(),
	}); err != nil {
		t.Fatalf("upsert %s: %v", key, err)
	}
}

func postRetrieve(t *testing.T, srv string, body map[string]any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv+"/v1/retrieve", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func recallDeps(st *store.Store, handle string, emb embed.Embedder) Deps {
	return Deps{
		Store:             st,
		Retrieval:         &retrieval.Engine{Store: st, Embedder: emb},
		Activity:          activity.New(4),
		DefaultUser:       handle,
		RecallMinScore:    0.5,
		RecallMaxDistance: 0.6,
	}
}

func TestRetrieveRecordsWhatItRecalled(t *testing.T) {
	st, handle, uid, emb := recallFixture(t)
	embeddedFact(t, st, emb, uid, "pet.quokka", "the quokka naps")
	srv := testServer(t, recallDeps(st, handle, emb))

	if code := postRetrieve(t, srv.URL, map[string]any{"prompt": "quokka"}); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	got, err := st.RecallWindow(context.Background(), uid, 7)
	if err != nil {
		t.Fatal(err)
	}
	if want := (store.RecallCounts{Days: 7, Prompts: 1, Recalled: 1}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestRetrieveRecordsAMissWhenTheOnlyMemoryIsAboutSomethingElse(t *testing.T) {
	st, handle, uid, emb := recallFixture(t)
	embeddedFact(t, st, emb, uid, "pet.wombat", "the wombat digs")
	srv := testServer(t, recallDeps(st, handle, emb))

	if code := postRetrieve(t, srv.URL, map[string]any{"prompt": "quokka"}); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	got, err := st.RecallWindow(context.Background(), uid, 7)
	if err != nil {
		t.Fatal(err)
	}
	if want := (store.RecallCounts{Days: 7, Prompts: 1}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// With no embedder there is no absolute number anywhere, which is the
// lexical-only local setup rather than a fault.
func TestRetrieveRecordsUngradedWithNoEmbedder(t *testing.T) {
	st, handle, uid, emb := recallFixture(t)
	embeddedFact(t, st, emb, uid, "pet.quokka", "the quokka naps")
	srv := testServer(t, recallDeps(st, handle, nil))

	if code := postRetrieve(t, srv.URL, map[string]any{"prompt": "quokka"}); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	got, err := st.RecallWindow(context.Background(), uid, 7)
	if err != nil {
		t.Fatal(err)
	}
	if want := (store.RecallCounts{Days: 7, Prompts: 1, Ungraded: 1}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// An empty prompt never searches, so counting it would put a prompt
// nobody asked into the denominator.
func TestRetrieveRecordsNothingForAnEmptyPrompt(t *testing.T) {
	st, handle, uid, emb := recallFixture(t)
	srv := testServer(t, recallDeps(st, handle, emb))

	if code := postRetrieve(t, srv.URL, map[string]any{"prompt": "  "}); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	got, err := st.RecallWindow(context.Background(), uid, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompts != 0 {
		t.Errorf("an empty prompt was counted: %+v", got)
	}
}

// Bookkeeping must never cost a session its memory. This is the same
// rule as artifact lookup failing quietly rather than failing the prompt.
func TestRetrieveStillAnswersWhenTheTallyCannotBeWritten(t *testing.T) {
	admin := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	adminStore, err := store.Open(ctx, admin)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer adminStore.Close()
	name := "anamnesia_recall_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := adminStore.Pool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", name)); err != nil {
		t.Skipf("cannot create a throwaway database (%v); skipping", err)
	}
	t.Cleanup(func() {
		_, _ = adminStore.Pool.Exec(context.Background(),
			fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name

	st, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open throwaway: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// The tally is now impossible to write, which is what a retrieval
	// has to survive.
	if _, err := st.Pool.Exec(ctx, "DROP TABLE recall_daily"); err != nil {
		t.Fatalf("drop recall_daily: %v", err)
	}
	handle := "recall-broken"
	uid, err := st.EnsureUser(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	emb := keywordEmbedder{keywords: []string{"quokka", "wombat"}}
	embeddedFact(t, st, emb, uid, "pet.quokka", "the quokka naps")
	srv := testServer(t, recallDeps(st, handle, emb))

	b, _ := json.Marshal(map[string]any{"prompt": "quokka"})
	resp, err := http.Post(srv.URL+"/v1/retrieve", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a failed tally lost the session its memory", resp.StatusCode)
	}
	var out RetrieveResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Hits) == 0 {
		t.Error("no hits came back")
	}
}

// The stub embedder hashes text to a random unit vector, so a prompt and
// the memory that answers it sit about as far apart as two unrelated
// texts do. Judging that would report a permanent nothing-recalled on
// every install that has not configured an embedding model, which is the
// default one.
func TestGradeRecallDoesNotJudgeByAStubsDistances(t *testing.T) {
	hits := []anamnesia.SearchHit{hitAt(dist(0.0))}
	bars := recallBars{MinScore: 0.5, MaxDistance: 0.6, TrustDistance: false}
	if got := gradeRecall(hits, bars); got != store.RecallUngraded {
		t.Errorf("got %q, want %q: a stub's distance is not evidence", got, store.RecallUngraded)
	}
}

// A reranker scores the text, not the embedding, so it is still a
// judgement even where the distances are not.
func TestGradeRecallStillUsesTheRerankerWithoutTrustworthyDistances(t *testing.T) {
	h := hitAt(dist(0.0))
	h.RerankerRank = 1
	h.Score = 0.9
	bars := recallBars{MinScore: 0.5, MaxDistance: 0.6, TrustDistance: false}
	if got := gradeRecall([]anamnesia.SearchHit{h}, bars); got != store.RecallHit {
		t.Errorf("got %q, want %q", got, store.RecallHit)
	}
}

func TestRetrieveRecordsUngradedWhenTheEmbedderIsAStub(t *testing.T) {
	st, handle, uid, emb := recallFixture(t)
	embeddedFact(t, st, emb, uid, "pet.quokka", "the quokka naps")
	deps := recallDeps(st, handle, emb)
	deps.EmbedProvider = "stub"
	srv := testServer(t, deps)

	if code := postRetrieve(t, srv.URL, map[string]any{"prompt": "quokka"}); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	got, err := st.RecallWindow(context.Background(), uid, 7)
	if err != nil {
		t.Fatal(err)
	}
	if want := (store.RecallCounts{Days: 7, Prompts: 1, Ungraded: 1}); got != want {
		t.Errorf("got %+v, want %+v: a stub install graded itself", got, want)
	}
}

// ── /v1/stats ────────────────────────────────────────────────────────

func TestStatsReportsTheRecallTally(t *testing.T) {
	st, scope, _, _, base := dbServer(t, nil)
	ctx := context.Background()
	for _, out := range []store.RecallOutcome{store.RecallHit, store.RecallHit, store.RecallMiss, store.RecallUngraded} {
		if err := st.RecordRetrieval(ctx, scope.UserID, out); err != nil {
			t.Fatal(err)
		}
	}

	var got map[string]any
	if code := getJSON(t, base+"/v1/stats", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	recall, ok := got["recall"].(map[string]any)
	if !ok {
		t.Fatalf("stats carries no recall block: %v", got)
	}
	for field, want := range map[string]float64{"days": 7, "prompts": 4, "recalled": 2, "ungraded": 1} {
		if recall[field] != want {
			t.Errorf("recall.%s = %v, want %v", field, recall[field], want)
		}
	}
}

// The tally is kept per user, so a project filter narrows everything
// else and leaves this alone rather than reporting zero.
func TestStatsReportsTheRecallTallyUnderAProjectFilterToo(t *testing.T) {
	st, scope, handle, slug, base := dbServer(t, nil)
	if err := st.RecordRetrieval(context.Background(), scope.UserID, store.RecallHit); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if code := getJSON(t, base+"/v1/stats?user="+handle+"&project="+slug, &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	recall := got["recall"].(map[string]any)
	if recall["recalled"] != float64(1) {
		t.Errorf("recall.recalled = %v, want 1", recall["recalled"])
	}
}
