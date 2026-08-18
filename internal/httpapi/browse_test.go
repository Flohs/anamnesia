package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

type listBody struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

func TestFactsListPagesWithACursor(t *testing.T) {
	st, scope, _, _, base := dbServer(t, nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := st.UpsertFact(ctx, &anamnesia.Fact{
			Scope: scope, FactKind: anamnesia.FactScopeProject,
			Key: fmt.Sprintf("key.%d", i), Value: map[string]any{"v": i}, Trust: 0.7,
		}); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 5; page++ {
		u := base + "/v1/facts?limit=2"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		var got listBody
		if code := getJSON(t, u, &got); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		for _, item := range got.Items {
			id := item["id"].(string)
			if seen[id] {
				t.Errorf("fact %s came back on two pages", id)
			}
			seen[id] = true
		}
		if got.NextCursor == nil {
			break
		}
		cursor = *got.NextCursor
	}
	if len(seen) != 3 {
		t.Errorf("saw %d facts across the pages, want 3", len(seen))
	}
}

func TestFactsListNeverCarriesAnEmbedding(t *testing.T) {
	st, scope, _, _, base := dbServer(t, nil)
	if err := st.UpsertFact(context.Background(), &anamnesia.Fact{
		Scope: scope, FactKind: anamnesia.FactScopeProject,
		Key: "deploy.target", Value: map[string]any{"v": "fly.io"}, Trust: 0.7,
	}); err != nil {
		t.Fatal(err)
	}
	var got listBody
	if code := getJSON(t, base+"/v1/facts", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(got.Items))
	}
	if _, ok := got.Items[0]["embedding"]; ok {
		t.Error("a 1536-float vector has no business on the wire")
	}
	if got.Items[0]["key"] != "deploy.target" {
		t.Errorf("item = %v", got.Items[0])
	}
}

func TestSourcesListFiltersByState(t *testing.T) {
	st, scope, _, _, base := dbServer(t, nil)
	ctx := context.Background()
	pending := &anamnesia.Source{Scope: scope, Kind: "chat-turn", RawContent: "waiting"}
	skipped := &anamnesia.Source{Scope: scope, Kind: "chat-turn", RawContent: "passed over"}
	for _, s := range []*anamnesia.Source{pending, skipped} {
		if err := st.InsertSource(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkSkipped(ctx, skipped.ID); err != nil {
		t.Fatal(err)
	}

	var got listBody
	if code := getJSON(t, base+"/v1/sources?state=skipped", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(got.Items) != 1 || got.Items[0]["id"] != skipped.ID.String() {
		t.Errorf("items = %v, want only the skipped source", got.Items)
	}
}

func TestExperienceDetailAndUnknownId(t *testing.T) {
	st, scope, _, _, base := dbServer(t, nil)
	exp := &anamnesia.Experience{
		Scope: scope, Kind: anamnesia.ExperienceCase,
		Title: "chose pnpm", Body: "the team standardised on pnpm",
	}
	if err := st.RecordExperience(context.Background(), exp); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if code := getJSON(t, base+"/v1/experiences/"+exp.ID.String(), &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got["title"] != "chose pnpm" {
		t.Errorf("experience = %v", got)
	}
	if code := getJSON(t, base+"/v1/experiences/8d4e4ec4-1f7f-4a6a-9d5e-2b6d1f0d2f11", nil); code != http.StatusNotFound {
		t.Errorf("unknown id status = %d, want 404", code)
	}
	if code := getJSON(t, base+"/v1/experiences/not-a-uuid", nil); code != http.StatusBadRequest {
		t.Errorf("malformed id status = %d, want 400", code)
	}
}

func TestEveryDomainListAnswers(t *testing.T) {
	_, _, _, _, base := dbServer(t, nil)
	for _, domain := range []string{
		"facts", "experiences", "skills", "entities", "edges", "sources", "working",
	} {
		var got listBody
		if code := getJSON(t, base+"/v1/"+domain, &got); code != http.StatusOK {
			t.Errorf("/v1/%s: status = %d, want 200", domain, code)
			continue
		}
		if got.Items == nil {
			t.Errorf("/v1/%s: items is null, want an empty list", domain)
		}
	}
}

func TestBadCursorIsARequestError(t *testing.T) {
	_, _, _, _, base := dbServer(t, nil)
	if code := getJSON(t, base+"/v1/facts?cursor=not-a-cursor", nil); code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

func TestActivityBucketsEndpoint(t *testing.T) {
	st, scope, _, _, base := dbServer(t, nil)
	if err := st.InsertSource(context.Background(), &anamnesia.Source{
		Scope: scope, Kind: "chat-turn", RawContent: "content",
	}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Buckets []map[string]any `json:"buckets"`
	}
	if code := getJSON(t, base+"/v1/stats/activity?days=7", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(got.Buckets) != 1 {
		t.Fatalf("buckets = %v, want one day", got.Buckets)
	}
	if got.Buckets[0]["sources"] != float64(1) {
		t.Errorf("bucket = %v, want one source", got.Buckets[0])
	}
}

func TestEmbeddingMapEndpoint(t *testing.T) {
	st, scope, _, _, base := dbServer(t, nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		exp := &anamnesia.Experience{
			Scope: scope, Kind: anamnesia.ExperienceCase,
			Title: fmt.Sprintf("experience %d", i), Body: "body",
		}
		if err := st.RecordExperience(ctx, exp); err != nil {
			t.Fatal(err)
		}
		vec := make([]float32, 1536)
		for j := range vec {
			vec[j] = float32((i+1)*(j%5)) / 10
		}
		if err := st.SetExperienceEmbedding(ctx, exp.ID, vec, "test"); err != nil {
			t.Fatal(err)
		}
	}

	var got map[string]any
	if code := getJSON(t, base+"/v1/embedding-map?domain=experiences", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got["n"] != float64(3) || got["dims"] != float64(1536) {
		t.Errorf("map = n %v dims %v, want 3 and 1536", got["n"], got["dims"])
	}
	variance := got["explained_variance"].([]any)
	if len(variance) != 2 {
		t.Fatalf("explained_variance = %v, want two components", variance)
	}
	if variance[0].(float64) <= 0 {
		t.Errorf("first component explains %v, want something", variance[0])
	}
	points := got["points"].([]any)
	if len(points) != 3 {
		t.Fatalf("points = %d, want 3", len(points))
	}
	first := points[0].(map[string]any)
	if _, ok := first["x"]; !ok {
		t.Errorf("point = %v, want coordinates", first)
	}
}

func TestEmbeddingMapRejectsAnUnknownDomain(t *testing.T) {
	_, _, _, _, base := dbServer(t, nil)
	if code := getJSON(t, base+"/v1/embedding-map?domain=skills", nil); code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 naming the domains that have vectors", code)
	}
}
