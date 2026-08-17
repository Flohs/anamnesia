package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/flohs/anamnesia/internal/llm"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// Reranker re-orders an existing candidate set against the query.
// Implementations: Cohere or no-op.
type Reranker interface {
	Rerank(ctx context.Context, query string, hits []anamnesia.SearchHit) ([]anamnesia.SearchHit, error)
}

// NewReranker returns a Reranker for the named provider. provider="" or
// "none" returns nil, signalling "no reranker — skip the step".
func NewReranker(provider, apiKey, model string) (Reranker, error) {
	switch provider {
	case "", "none":
		return nil, nil
	case "cohere":
		if apiKey == "" {
			return nil, errors.New("cohere: api key required")
		}
		return NewCohere(apiKey, model), nil
	case "openrouter":
		if apiKey == "" {
			return nil, errors.New("openrouter: api key required")
		}
		return NewOpenRouter(apiKey, model), nil
	default:
		return nil, errors.New("rerank: unknown provider " + provider)
	}
}

// apiReranker calls a Cohere-shaped rerank endpoint. Both Cohere's own
// API and OpenRouter's /rerank proxy speak the same request/response
// shape, so we parameterize URL + auth + optional headers and share
// one implementation.
type apiReranker struct {
	URL          string
	APIKey       string
	Model        string
	Label        string // prefix used in error messages, e.g. "cohere rerank"
	ExtraHeaders map[string]string
	HTTP         *http.Client
}

// NewCohere returns a reranker that talks to Cohere's Rerank v3 endpoint.
func NewCohere(key, model string) Reranker {
	if model == "" {
		model = "rerank-english-v3.0"
	}
	return &apiReranker{
		URL:    "https://api.cohere.ai/v1/rerank",
		APIKey: key,
		Model:  model,
		Label:  "cohere rerank",
		HTTP:   &http.Client{Timeout: 10 * time.Second},
	}
}

// NewOpenRouter returns a reranker that talks to OpenRouter's /rerank
// proxy. Model slugs are vendor-prefixed (e.g. "cohere/rerank-v3.5").
func NewOpenRouter(key, model string) Reranker {
	if model == "" {
		model = "cohere/rerank-v3.5"
	}
	return &apiReranker{
		URL:          llm.OpenRouterBaseURL + "/rerank",
		APIKey:       key,
		Model:        model,
		Label:        "openrouter rerank",
		ExtraHeaders: llm.OpenRouterHeaders(),
		HTTP:         &http.Client{Timeout: 10 * time.Second},
	}
}

type rerankReq struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	Model     string   `json:"model"`
	TopN      int      `json:"top_n"`
}

type rerankResp struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank scores each candidate against the query and returns them sorted
// by relevance_score descending. The original RRF score is replaced; the
// new ordering wins.
func (c *apiReranker) Rerank(ctx context.Context, query string, hits []anamnesia.SearchHit) ([]anamnesia.SearchHit, error) {
	if len(hits) == 0 {
		return hits, nil
	}
	docs := make([]string, len(hits))
	for i, h := range hits {
		docs[i] = h.Body()
	}
	body, err := json.Marshal(rerankReq{
		Query: query, Documents: docs, Model: c.Model, TopN: len(docs),
	})
	if err != nil {
		return hits, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return hits, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return hits, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return hits, fmt.Errorf("%s: %s: %s", c.Label, resp.Status, raw)
	}
	var out rerankResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return hits, err
	}
	reranked := make([]anamnesia.SearchHit, 0, len(out.Results))
	for r, item := range out.Results {
		if item.Index < 0 || item.Index >= len(hits) {
			continue
		}
		h := hits[item.Index]
		h.RerankerRank = r + 1
		h.Score = item.RelevanceScore
		reranked = append(reranked, h)
	}
	sort.SliceStable(reranked, func(i, j int) bool { return reranked[i].Score > reranked[j].Score })
	return reranked, nil
}
