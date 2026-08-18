// eval.go — `anamnesia eval`: ingest a fixture corpus through the real
// pipeline, run labelled queries, and report what retrieval returned.
//
// It talks HTTP like any other client rather than reaching into the
// store, so it measures the path a hook actually takes. The only direct
// database access is deleting the scope it created.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/flohs/anamnesia/internal/httpapi"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

const (
	// evalUser is a handle no real person would choose: the teardown
	// deletes everything under it, so evalScopeExists refuses to run
	// if it's ever occupied by an actual user.
	evalUser    = "anamnesia-eval"
	evalProject = "eval-corpus"
	// drainTimeout bounds the wait for extraction and embedding. A run
	// scored against a half-warm index is worse than no run.
	drainTimeout = 10 * time.Minute
)

type evalReport struct {
	At        string         `json:"at"`
	Queries   int            `json:"queries"`
	Aggregate aggregateScore `json:"aggregate"`
	PerQuery  []queryScore   `json:"per_query"`
}

// runEvalPass ingests the corpus, waits for the queues to drain, runs
// every query and scores the result.
func runEvalPass(ctx context.Context, hc *hostConfig, sources []evalSource, queries []evalQuery, k int) (evalReport, error) {
	c := &evalClient{base: hc.ServerURL(), token: hc.Token(), hc: &http.Client{Timeout: 60 * time.Second}}

	// corpus id -> the source_id the server assigned, which is what hits
	// carry and therefore what scoring compares against.
	byCorpusID := make(map[string]string, len(sources))
	for _, s := range sources {
		occurred := s.OccurredAt
		var resp httpapi.IngestResponse
		err := c.post(ctx, "/v1/ingest", httpapi.IngestRequest{
			User: evalUser, Project: evalProject, Kind: s.Kind,
			ExternalRef: s.ID, OccurredAt: &occurred, Content: s.Content,
			PreserveRaw: true,
		}, &resp)
		if err != nil {
			return evalReport{}, fmt.Errorf("ingest %s: %w", s.ID, err)
		}
		byCorpusID[s.ID] = resp.SourceID.String()
	}

	if err := c.waitForDrain(ctx, drainTimeout); err != nil {
		return evalReport{}, err
	}

	ks := []int{1, 5, 10}
	scores := make([]queryScore, 0, len(queries))
	for _, q := range queries {
		var resp httpapi.RetrieveResp
		started := time.Now()
		err := c.post(ctx, "/v1/retrieve", httpapi.HookEvent{
			User: evalUser, Project: evalProject, Prompt: q.Text, K: k, OnlyRaw: true,
		}, &resp)
		if err != nil {
			return evalReport{}, fmt.Errorf("retrieve %s: %w", q.ID, err)
		}
		latency := time.Since(started).Milliseconds()

		ranked := make([]string, 0, len(resp.Hits))
		for _, h := range resp.Hits {
			if id := hitSourceID(h); id != "" {
				ranked = append(ranked, id)
			}
		}
		want := make([]string, 0, len(q.Relevant))
		for _, r := range q.Relevant {
			want = append(want, byCorpusID[r])
		}
		scores = append(scores, score(q.ID, ranked, want, ks, latency))
	}

	return evalReport{
		Queries:   len(queries),
		Aggregate: aggregate(scores, ks),
		PerQuery:  scores,
	}, nil
}

// hitSourceID reports which source a hit came from. A hit with no source
// was not extracted from the corpus — a consolidation summary, say — and
// cannot be scored against source-granularity labels.
func hitSourceID(h anamnesia.SearchHit) string {
	switch {
	case h.Fact != nil && h.Fact.SourceID != nil:
		return h.Fact.SourceID.String()
	case h.Experience != nil && h.Experience.SourceID != nil:
		return h.Experience.SourceID.String()
	}
	return ""
}

type evalClient struct {
	base  string
	token string
	hc    *http.Client
}

func (c *evalClient) post(ctx context.Context, path string, body, into any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, bytes.TrimSpace(msg))
	}
	if into == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// waitForDrain blocks until nothing is pending. Extraction is async, so
// querying before it finishes measures an index that is still filling.
func (c *evalClient) waitForDrain(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			c.base+"/v1/queue/pending?user="+evalUser+"&project="+evalProject, nil)
		if err != nil {
			return err
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		var q httpapi.QueuePendingResponse
		err = json.NewDecoder(resp.Body).Decode(&q)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if q.ExtractPending == 0 && q.EmbedPending == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("queues did not drain within %s (%d to extract, %d to embed).\n"+
				"Check `anamnesia logs`: an unconfigured or failing model leaves sources pending forever",
				timeout, q.ExtractPending, q.EmbedPending)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// evalScopeExists reports whether evalUser already denotes a real user
// handle. The teardown deletes by that fixed handle, so if a real user
// ever held it, running the eval would delete their entire memory.
// Refusing to run is recoverable; deleting a stranger's memory is not.
func evalScopeExists(ctx context.Context, hc *hostConfig) (bool, error) {
	st, err := store.Open(ctx, hc.DatabaseURL())
	if err != nil {
		return false, err
	}
	defer st.Close()
	_, found, err := st.LookupUser(ctx, evalUser)
	return found, err
}

// deleteEvalScope removes everything the run created. commitments.user_id
// has no ON DELETE CASCADE (unlike every other memory table), so deleting
// the user first would abort atomically on any commitments row the eval
// corpus produced; delete those explicitly first.
func deleteEvalScope(ctx context.Context, hc *hostConfig) error {
	st, err := store.Open(ctx, hc.DatabaseURL())
	if err != nil {
		return err
	}
	defer st.Close()

	id, found, err := st.LookupUser(ctx, evalUser)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM commitments WHERE user_id = $1`, id); err != nil {
		return fmt.Errorf("delete eval commitments: %w", err)
	}

	_, err = st.DeleteUser(ctx, evalUser)
	return err
}
