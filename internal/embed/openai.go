package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// openAIEmbedder calls the OpenAI-compatible embeddings endpoint at
// {baseURL}/embeddings. baseURL is configurable so users can point this
// at OpenRouter, vLLM, Azure OpenAI, Ollama, etc.
type openAIEmbedder struct {
	baseURL string
	apiKey  string
	model   string
	dims    int
	hc      *http.Client
}

func (o *openAIEmbedder) Dims() int     { return o.dims }
func (o *openAIEmbedder) Model() string { return o.model }

func (o *openAIEmbedder) client() *http.Client {
	if o.hc == nil {
		o.hc = &http.Client{Timeout: 60 * time.Second}
	}
	return o.hc
}

type oaiReq struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions *int     `json:"dimensions,omitempty"`
}

type oaiResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// maxEmbedChars is a defensive cap on input length per item. OpenAI's
// embedding models top out at 8192 tokens. Worst-case (dense Unicode,
// long JSON values) the ratio is ~2 chars/token; we clip at 16000 chars
// to stay safely under the limit even in adversarial inputs. A few
// hundred chars from the tail of a long experience body don't change
// retrieval rank meaningfully.
const maxEmbedChars = 16000

func (o *openAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	clipped := make([]string, len(texts))
	for i, t := range texts {
		if len(t) > maxEmbedChars {
			clipped[i] = t[:maxEmbedChars]
		} else {
			clipped[i] = t
		}
	}
	body := oaiReq{Model: o.model, Input: clipped}
	// text-embedding-3-* support arbitrary dims via MRL. Older models
	// reject the field; we only forward dims if it looks like a 3-series
	// model name.
	if o.dims > 0 && isMRLModel(o.model) {
		d := o.dims
		body.Dimensions = &d
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		o.baseURL+"/embeddings", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	req.Header.Set("Content-Type", "application/json")

	res, err := o.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai embed: %w", err)
	}
	defer res.Body.Close()
	rb, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("openai embed: %s: %s", res.Status, string(rb))
	}
	var parsed oaiResp
	if err := json.Unmarshal(rb, &parsed); err != nil {
		return nil, fmt.Errorf("openai embed: parse %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("openai embed: %s", parsed.Error.Message)
	}
	out := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(out) {
			continue
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}

func isMRLModel(m string) bool {
	switch m {
	case "text-embedding-3-small", "text-embedding-3-large":
		return true
	}
	return false
}
