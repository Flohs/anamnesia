// Package config loads runtime configuration from environment
// variables. Single-tenant edition: no OAuth, no JWKS, no tenant slugs.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved runtime configuration. Environment variables
// are read in Load(); zero values use sane defaults.
type Config struct {
	// Server
	HTTPAddr     string        // ANAMNESIA_HTTP_ADDR (default ":8181")
	ServerToken  string        // ANAMNESIA_SERVER_TOKEN (optional shared secret for /v1 + /mcp)
	WorkerInProc bool          // ANAMNESIA_WORKER_IN_PROCESS (default true)
	ShutdownWait time.Duration // ANAMNESIA_SHUTDOWN_WAIT (default 30s)

	// Database
	DatabaseURL string // ANAMNESIA_DATABASE_URL (required)

	// Default identity. When the HTTP API receives a request without an
	// override header, use these.
	DefaultUser    string // ANAMNESIA_DEFAULT_USER (default "default")
	DefaultProject string // ANAMNESIA_DEFAULT_PROJECT (optional)

	// Embeddings
	EmbedProvider string // "openai" | "openrouter" | "stub" (default depends on OPENROUTER_API_KEY)
	EmbedModel    string // ANAMNESIA_EMBED_MODEL (default depends on provider)
	EmbedDims     int    // ANAMNESIA_EMBED_DIMS (default 1536)
	OpenAIAPIKey  string // OPENAI_API_KEY
	OpenAIBaseURL string // OPENAI_BASE_URL (default https://api.openai.com/v1)

	// LLM (consolidation + extraction worker)
	LLMProvider     string // "anthropic" | "openai" | "openrouter" | "stub" (default depends on OPENROUTER_API_KEY)
	LLMModel        string // ANAMNESIA_LLM_MODEL (default depends on provider)
	AnthropicAPIKey string // ANTHROPIC_API_KEY

	// Reranker
	RerankProvider string // "" | "none" | "cohere" | "openrouter" (default depends on OPENROUTER_API_KEY)
	RerankModel    string // ANAMNESIA_RERANK_MODEL (default depends on provider)
	CohereAPIKey   string // COHERE_API_KEY

	// OpenRouter
	// One API key + one base URL fronts chat, embeddings, and rerank.
	// When OPENROUTER_API_KEY is set and no other provider is explicitly
	// chosen, all three workloads auto-default to "openrouter".
	OpenRouterAPIKey string // OPENROUTER_API_KEY

	// PII detector
	PIIProvider    string // "" | "none" | "regex" | "presidio" (default "regex")
	PIIMode        string // "tag" | "redact" (default "tag")
	PresidioURL    string // ANAMNESIA_PRESIDIO_URL

	// Worker
	ConsolidateEvery time.Duration // ANAMNESIA_CONSOLIDATE_EVERY (default 24h)
	ForgetEvery      time.Duration // ANAMNESIA_FORGET_EVERY (default 1h)
	DecayEvery       time.Duration // ANAMNESIA_DECAY_EVERY (default 1h)
	ExtractEvery     time.Duration // ANAMNESIA_EXTRACT_EVERY (default 15s)
	EmbedBackfill    time.Duration // ANAMNESIA_EMBED_BACKFILL (default 1m)

	// ExtractCommitments lets the extractor emit ADD_COMMITMENT ops.
	// ANAMNESIA_EXTRACT_COMMITMENTS (default false).
	ExtractCommitments bool
}

// Load reads the environment and produces a Config.
func Load() (*Config, error) {
	orKey := os.Getenv("OPENROUTER_API_KEY")
	hasOR := strings.TrimSpace(orKey) != ""

	// Provider auto-light-up: with OPENROUTER_API_KEY set, every workload
	// whose provider env var is unset defaults to "openrouter". Explicit
	// settings (including "stub" / "none") win.
	embedProvider := providerDefault(os.Getenv("ANAMNESIA_EMBED_PROVIDER"), hasOR, "openrouter", "stub")
	llmProvider := providerDefault(os.Getenv("ANAMNESIA_LLM_PROVIDER"), hasOR, "openrouter", "stub")
	rerankProvider := providerDefault(os.Getenv("ANAMNESIA_RERANK_PROVIDER"), hasOR, "openrouter", "none")

	c := &Config{
		HTTPAddr:         getenv("ANAMNESIA_HTTP_ADDR", ":8181"),
		ServerToken:      os.Getenv("ANAMNESIA_SERVER_TOKEN"),
		WorkerInProc:     parseBool(os.Getenv("ANAMNESIA_WORKER_IN_PROCESS"), true),
		ShutdownWait:     parseDuration(os.Getenv("ANAMNESIA_SHUTDOWN_WAIT"), 30*time.Second),
		DatabaseURL:      os.Getenv("ANAMNESIA_DATABASE_URL"),
		DefaultUser:      getenv("ANAMNESIA_DEFAULT_USER", "default"),
		DefaultProject:   os.Getenv("ANAMNESIA_DEFAULT_PROJECT"),
		EmbedProvider:    embedProvider,
		EmbedModel:       getenv("ANAMNESIA_EMBED_MODEL", defaultEmbedModel(embedProvider)),
		EmbedDims:        parseInt(os.Getenv("ANAMNESIA_EMBED_DIMS"), 1536),
		OpenAIAPIKey:     os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:    getenv("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		LLMProvider:      llmProvider,
		LLMModel:         getenv("ANAMNESIA_LLM_MODEL", defaultLLMModel(llmProvider)),
		AnthropicAPIKey:  os.Getenv("ANTHROPIC_API_KEY"),
		RerankProvider:   rerankProvider,
		RerankModel:      getenv("ANAMNESIA_RERANK_MODEL", defaultRerankModel(rerankProvider)),
		CohereAPIKey:     os.Getenv("COHERE_API_KEY"),
		OpenRouterAPIKey: orKey,
		PIIProvider:      getenv("ANAMNESIA_PII_PROVIDER", "regex"),
		PIIMode:          getenv("ANAMNESIA_PII_MODE", "tag"),
		PresidioURL:      os.Getenv("ANAMNESIA_PRESIDIO_URL"),
		ConsolidateEvery: parseDuration(os.Getenv("ANAMNESIA_CONSOLIDATE_EVERY"), 24*time.Hour),
		ForgetEvery:      parseDuration(os.Getenv("ANAMNESIA_FORGET_EVERY"), time.Hour),
		DecayEvery:       parseDuration(os.Getenv("ANAMNESIA_DECAY_EVERY"), time.Hour),
		ExtractEvery:     parseDuration(os.Getenv("ANAMNESIA_EXTRACT_EVERY"), 15*time.Second),
		EmbedBackfill:    parseDuration(os.Getenv("ANAMNESIA_EMBED_BACKFILL"), time.Minute),

		ExtractCommitments: parseBool(os.Getenv("ANAMNESIA_EXTRACT_COMMITMENTS"), false),
	}

	if strings.TrimSpace(c.DatabaseURL) == "" {
		return nil, errors.New("ANAMNESIA_DATABASE_URL is required")
	}

	if c.EmbedProvider == "openai" && c.OpenAIAPIKey == "" {
		return nil, errors.New("OPENAI_API_KEY is required when ANAMNESIA_EMBED_PROVIDER=openai")
	}
	if c.EmbedProvider == "openrouter" && c.OpenRouterAPIKey == "" {
		return nil, errors.New("OPENROUTER_API_KEY is required when ANAMNESIA_EMBED_PROVIDER=openrouter")
	}
	if c.LLMProvider == "anthropic" && c.AnthropicAPIKey == "" {
		return nil, errors.New("ANTHROPIC_API_KEY is required when ANAMNESIA_LLM_PROVIDER=anthropic")
	}
	if c.LLMProvider == "openai" && c.OpenAIAPIKey == "" {
		return nil, errors.New("OPENAI_API_KEY is required when ANAMNESIA_LLM_PROVIDER=openai")
	}
	if c.LLMProvider == "openrouter" && c.OpenRouterAPIKey == "" {
		return nil, errors.New("OPENROUTER_API_KEY is required when ANAMNESIA_LLM_PROVIDER=openrouter")
	}
	if c.RerankProvider == "cohere" && c.CohereAPIKey == "" {
		return nil, errors.New("COHERE_API_KEY is required when ANAMNESIA_RERANK_PROVIDER=cohere")
	}
	if c.RerankProvider == "openrouter" && c.OpenRouterAPIKey == "" {
		return nil, errors.New("OPENROUTER_API_KEY is required when ANAMNESIA_RERANK_PROVIDER=openrouter")
	}
	if c.PIIProvider == "presidio" && c.PresidioURL == "" {
		return nil, errors.New("ANAMNESIA_PRESIDIO_URL is required when ANAMNESIA_PII_PROVIDER=presidio")
	}

	return c, nil
}

// providerDefault picks the default provider value. Explicit settings
// (anything non-empty, including "stub" or "none") win unconditionally;
// when nothing is set, return ifOR if the auto-light-up condition is true
// and otherwise the plain default.
func providerDefault(explicit string, autoOR bool, ifOR, plain string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		return v
	}
	if autoOR {
		return ifOR
	}
	return plain
}

// defaultLLMModel picks a sensible default per provider so the user
// only needs to set the provider + key to get going.
func defaultLLMModel(provider string) string {
	switch provider {
	case "anthropic":
		return "claude-sonnet-4-6"
	case "openai":
		return "gpt-4o-mini"
	case "openrouter":
		// OpenRouter slugs use dot-separated versions (vs Anthropic's
		// dash-separated direct-API names).
		return "anthropic/claude-sonnet-4.6"
	default:
		return "stub"
	}
}

// defaultEmbedModel picks a sensible default per embed provider.
func defaultEmbedModel(provider string) string {
	switch provider {
	case "openrouter":
		return "openai/text-embedding-3-small"
	default:
		return "text-embedding-3-small"
	}
}

// defaultRerankModel picks a sensible default per rerank provider.
func defaultRerankModel(provider string) string {
	switch provider {
	case "openrouter":
		return "cohere/rerank-v3.5"
	default:
		return "rerank-english-v3.0"
	}
}

func getenv(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}

func parseBool(v string, def bool) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func parseInt(v string, def int) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func parseDuration(v string, def time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// String returns a redacted single-line summary suitable for logs.
func (c *Config) String() string {
	return fmt.Sprintf(
		"http=%s db=%s embed=%s/%s/%d llm=%s/%s",
		c.HTTPAddr, redact(c.DatabaseURL),
		c.EmbedProvider, c.EmbedModel, c.EmbedDims,
		c.LLMProvider, c.LLMModel,
	)
}

func redact(dsn string) string {
	if i := strings.Index(dsn, "@"); i >= 0 {
		if j := strings.LastIndex(dsn[:i], ":"); j >= 0 {
			return dsn[:j+1] + "***" + dsn[i:]
		}
	}
	return dsn
}
