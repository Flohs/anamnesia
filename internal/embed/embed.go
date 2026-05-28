// Package embed provides vector embeddings for memory rows. Two
// implementations: openai (real model) and stub (deterministic in-process
// for tests + offline runs).
package embed

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"

	"github.com/flohs/anamnesia-open-source/internal/llm"
)

// Embedder converts a batch of strings into float32 vectors.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dims() int
	Model() string
}

// New returns an Embedder for the given provider.
func New(provider, model, baseURL, apiKey string, dims int) (Embedder, error) {
	switch provider {
	case "openai":
		if apiKey == "" {
			return nil, errors.New("openai embedder: OPENAI_API_KEY required")
		}
		return &openAIEmbedder{
			baseURL: baseURL,
			apiKey:  apiKey,
			model:   model,
			dims:    dims,
		}, nil
	case "openrouter":
		if apiKey == "" {
			return nil, errors.New("openrouter embedder: OPENROUTER_API_KEY required")
		}
		return &openAIEmbedder{
			baseURL:      llm.OpenRouterBaseURL,
			apiKey:       apiKey,
			model:        model,
			dims:         dims,
			extraHeaders: llm.OpenRouterHeaders(),
		}, nil
	case "stub", "":
		return &stubEmbedder{model: "stub", dims: dims}, nil
	default:
		return nil, errors.New("unknown embed provider: " + provider)
	}
}

// stubEmbedder hashes the text into a deterministic dense vector. Good
// enough for unit tests + offline mode where retrieval just needs
// "some similarity signal" to function.
type stubEmbedder struct {
	model string
	dims  int
}

func (s *stubEmbedder) Dims() int     { return s.dims }
func (s *stubEmbedder) Model() string { return s.model }

func (s *stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		vec := make([]float32, s.dims)
		h := sha256.Sum256([]byte(t))
		// Reuse the hash to seed a deterministic random sequence.
		for j := 0; j < s.dims; j++ {
			b := h[(j*4)%32]
			c := h[(j*4+1)%32]
			d := h[(j*4+2)%32]
			e := h[(j*4+3)%32]
			u := binary.BigEndian.Uint32([]byte{b, c, d, e})
			vec[j] = (float32(u)/float32(math.MaxUint32))*2 - 1
		}
		norm(vec)
		out[i] = vec
	}
	return out, nil
}

func norm(v []float32) {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return
	}
	inv := 1.0 / math.Sqrt(s)
	for i, x := range v {
		v[i] = float32(float64(x) * inv)
	}
}
