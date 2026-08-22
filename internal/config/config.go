// Package config loads the server's runtime configuration from environment
// variables. Single-tenant edition: no OAuth, no JWKS, no tenant slugs.
//
// On the host side the user edits ~/.anamnesia/config.toml and `anamnesia
// start` translates it into this environment for the server process, so
// this package stays the single place where a server setting is defined.
// Setting an ANAMNESIA_* variable directly still works and still wins,
// which is what keeps one-off overrides and CI runs simple.
//
// Malformed values are errors, never silent fallbacks: a typo in a
// duration or a dimension count used to be swallowed and replaced by the
// default, which is exactly the class of misconfiguration that is
// impossible to diagnose later.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved runtime configuration.
type Config struct {
	// Server
	HTTPAddr     string        // ANAMNESIA_HTTP_ADDR (default "127.0.0.1:8181")
	ServerToken  string        // ANAMNESIA_SERVER_TOKEN (optional shared secret for /v1 + /mcp)
	ShutdownWait time.Duration // ANAMNESIA_SHUTDOWN_WAIT (default 30s)

	// Database
	DatabaseURL string // ANAMNESIA_DATABASE_URL (required)

	// Default identity, used when a request carries no override.
	DefaultUser    string // ANAMNESIA_DEFAULT_USER (default "default")
	DefaultProject string // ANAMNESIA_DEFAULT_PROJECT (optional)

	// Embeddings
	EmbedProvider string // "openai" | "openrouter" | "stub"
	EmbedModel    string // ANAMNESIA_EMBED_MODEL
	EmbedDims     int    // ANAMNESIA_EMBED_DIMS (default 1536)
	OpenAIAPIKey  string // OPENAI_API_KEY
	OpenAIBaseURL string // OPENAI_BASE_URL

	// LLM (extraction + consolidation workers)
	LLMProvider     string        // "anthropic" | "openai" | "openrouter" | "stub"
	LLMModel        string        // ANAMNESIA_LLM_MODEL
	LLMHTTPTimeout  time.Duration // ANAMNESIA_LLM_HTTP_TIMEOUT (default 120s)
	AnthropicAPIKey string        // ANTHROPIC_API_KEY

	// Reranker
	RerankProvider string // "" | "none" | "cohere" | "openrouter"
	RerankModel    string // ANAMNESIA_RERANK_MODEL
	CohereAPIKey   string // COHERE_API_KEY

	// OpenRouter. One key and base URL fronts chat, embeddings and rerank.
	// When OPENROUTER_API_KEY is set and no other provider is explicitly
	// chosen, all three workloads default to "openrouter".
	OpenRouterAPIKey string // OPENROUTER_API_KEY

	// PII detector
	PIIProvider string // "" | "none" | "regex" | "presidio" (default "regex")
	PIIMode     string // "tag" | "redact" (default "tag")
	PresidioURL string // ANAMNESIA_PRESIDIO_URL

	// Worker cadences
	ConsolidateEvery time.Duration // ANAMNESIA_CONSOLIDATE_EVERY (default 24h)
	// ConsolidateSimilarity is the cosine two experiences must reach to
	// be folded into one insight. Range 0 to 1, which is fraction()'s
	// bound and the same bound settings.go enforces on the way in. A
	// value above 1 is unreachable by any cosine and would switch
	// consolidation off while still reporting healthy passes, which is
	// the failure this setting exists to make visible.
	ConsolidateSimilarity float64 // ANAMNESIA_CONSOLIDATE_SIMILARITY (default 0.65)
	// ArtifactMaxDistance is the cosine distance a published artifact
	// must be within to be offered beside an answer. Zero switches the
	// prompt-driven surface off.
	ArtifactMaxDistance   float64       // ANAMNESIA_ARTIFACT_MAX_DISTANCE (default 0.60)
	ConsolidateMaxCluster int           // ANAMNESIA_CONSOLIDATE_MAX_CLUSTER (default 8)
	ForgetEvery           time.Duration // ANAMNESIA_FORGET_EVERY (default 1h)
	DecayEvery            time.Duration // ANAMNESIA_DECAY_EVERY (default 1h)
	ExtractEvery          time.Duration // ANAMNESIA_EXTRACT_EVERY (default 15s)
	EmbedBackfill         time.Duration // ANAMNESIA_EMBED_BACKFILL (default 1m)

	// ExtractConcurrency is how many sources are extracted at once.
	ExtractConcurrency int // ANAMNESIA_EXTRACT_CONCURRENCY (default 1)

	// ExtractCommitments lets the extractor emit ADD_COMMITMENT ops.
	ExtractCommitments bool // ANAMNESIA_EXTRACT_COMMITMENTS (default false)

	// ExtractGraph lets the extractor run the graph pass (ADD_ENTITY /
	// ADD_EDGE) on a claude-session-graph source. GraphMaxOps caps how
	// many entities and edges one checkpoint may produce.
	ExtractGraph bool // ANAMNESIA_GRAPH_EXTRACT (default false)
	GraphMaxOps  int  // ANAMNESIA_GRAPH_MAX_OPS (default 12)

	// GraphCandidateDistance is the cosine distance within which an
	// existing entity (same kind) is offered to a newly extracted entity
	// as a possible match, triggering one extra disambiguation model
	// call. It does not merge anything by itself — the model decides
	// identity — so it is deliberately loose. 0 is a legitimate value
	// ("offer nothing"), so unlike the other numeric settings it is not
	// defaulted when zero — it always comes from
	// ANAMNESIA_GRAPH_CANDIDATE_DISTANCE.
	//
	// Range 0 to 1, which is fraction()'s bound below and, deliberately,
	// not the full 0-to-2 range of a cosine distance: past 1 the two
	// names point away from each other, and "less alike than unrelated"
	// is not a threshold for offering them as the same thing. The same
	// bound is enforced where the value is typed, by settings.go's
	// kFraction, so `anamnesia config set` and the server agree.
	GraphCandidateDistance float64 // ANAMNESIA_GRAPH_CANDIDATE_DISTANCE (default 0.45)

	// Decay half-lives per experience kind. Relevance falls by half over
	// this long since a memory was last used, per kind, which is what
	// makes an episode fade while an approach does not.
	DecayHalfLifeCase     time.Duration // ANAMNESIA_DECAY_HALF_LIFE_CASE (default 336h)
	DecayHalfLifeStrategy time.Duration // ANAMNESIA_DECAY_HALF_LIFE_STRATEGY (default 8760h)
	DecayHalfLifeHybrid   time.Duration // ANAMNESIA_DECAY_HALF_LIFE_HYBRID (default 1440h)

	// Activity recorder. ActivityEnabled off means no recorder at all,
	// which is also what makes the /v1/activity routes 404.
	ActivityEnabled bool // ANAMNESIA_ACTIVITY_ENABLED (default true)
	ActivityTraces  int  // ANAMNESIA_ACTIVITY_TRACES (default 200)
}

// DefaultEmbedDims is the shipped embedding width. It matches
// text-embedding-3-small and the schema built by the migrations.
const DefaultEmbedDims = 1536

// DefaultShutdownWait is how long the server may take to stop. The CLI
// waits for it to exit, so it has to know the same number: exported
// rather than written twice.
const DefaultShutdownWait = 30 * time.Second

// Load reads the environment and produces a Config. Every parse failure is
// reported rather than defaulted, and all of them are collected so one run
// surfaces every problem instead of one per attempt.
func Load() (*Config, error) {
	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	str := func(key, def string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return def
	}
	dur := func(key string, def time.Duration) time.Duration {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			fail("%s=%q is not a duration (try %q)", key, v, def.String())
			return def
		}
		if d <= 0 {
			fail("%s=%q must be positive", key, v)
			return def
		}
		return d
	}
	num := func(key string, def int) int {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			fail("%s=%q is not a number", key, v)
			return def
		}
		if n <= 0 {
			fail("%s=%q must be positive", key, v)
			return def
		}
		return n
	}
	boolean := func(key string, def bool) bool {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			fail("%s=%q is not a boolean (true/false)", key, v)
			return def
		}
		return b
	}
	fraction := func(key string, def float64) float64 {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			return def
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			fail("%s=%q is not a number", key, v)
			return def
		}
		// NaN passes every comparison below, and a NaN threshold makes
		// the comparisons that read it always false — inert, not loud.
		if math.IsNaN(f) || f < 0 || f > 1 {
			fail("%s=%q must be between 0 and 1", key, v)
			return def
		}
		return f
	}

	orKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	hasOR := orKey != ""

	embedProvider := providerDefault(os.Getenv("ANAMNESIA_EMBED_PROVIDER"), hasOR, "openrouter", "stub")
	llmProvider := providerDefault(os.Getenv("ANAMNESIA_LLM_PROVIDER"), hasOR, "openrouter", "stub")
	rerankProvider := providerDefault(os.Getenv("ANAMNESIA_RERANK_PROVIDER"), hasOR, "openrouter", "none")

	c := &Config{
		HTTPAddr:         str("ANAMNESIA_HTTP_ADDR", "127.0.0.1:8181"),
		ServerToken:      os.Getenv("ANAMNESIA_SERVER_TOKEN"),
		ShutdownWait:     dur("ANAMNESIA_SHUTDOWN_WAIT", DefaultShutdownWait),
		DatabaseURL:      os.Getenv("ANAMNESIA_DATABASE_URL"),
		DefaultUser:      str("ANAMNESIA_DEFAULT_USER", "default"),
		DefaultProject:   os.Getenv("ANAMNESIA_DEFAULT_PROJECT"),
		EmbedProvider:    embedProvider,
		EmbedModel:       str("ANAMNESIA_EMBED_MODEL", defaultEmbedModel(embedProvider)),
		EmbedDims:        num("ANAMNESIA_EMBED_DIMS", DefaultEmbedDims),
		OpenAIAPIKey:     os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:    str("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		LLMProvider:      llmProvider,
		LLMModel:         str("ANAMNESIA_LLM_MODEL", defaultLLMModel(llmProvider)),
		LLMHTTPTimeout:   dur("ANAMNESIA_LLM_HTTP_TIMEOUT", 120*time.Second),
		AnthropicAPIKey:  os.Getenv("ANTHROPIC_API_KEY"),
		RerankProvider:   rerankProvider,
		RerankModel:      str("ANAMNESIA_RERANK_MODEL", defaultRerankModel(rerankProvider)),
		CohereAPIKey:     os.Getenv("COHERE_API_KEY"),
		OpenRouterAPIKey: orKey,
		PIIProvider:      str("ANAMNESIA_PII_PROVIDER", "regex"),
		PIIMode:          str("ANAMNESIA_PII_MODE", "tag"),
		PresidioURL:      os.Getenv("ANAMNESIA_PRESIDIO_URL"),
		ConsolidateEvery: dur("ANAMNESIA_CONSOLIDATE_EVERY", 24*time.Hour),
		// Defaults repeated from jobs.DefaultConsolidate*; the agreement
		// is held by TestConsolidateDefaultsAgreeWithTheClusterer.
		ConsolidateSimilarity: fraction("ANAMNESIA_CONSOLIDATE_SIMILARITY", 0.65),
		ArtifactMaxDistance:   fraction("ANAMNESIA_ARTIFACT_MAX_DISTANCE", 0.60),
		ConsolidateMaxCluster: num("ANAMNESIA_CONSOLIDATE_MAX_CLUSTER", 8),
		ForgetEvery:           dur("ANAMNESIA_FORGET_EVERY", time.Hour),
		DecayEvery:            dur("ANAMNESIA_DECAY_EVERY", time.Hour),
		ExtractEvery:          dur("ANAMNESIA_EXTRACT_EVERY", 15*time.Second),
		EmbedBackfill:         dur("ANAMNESIA_EMBED_BACKFILL", time.Minute),

		ExtractConcurrency: num("ANAMNESIA_EXTRACT_CONCURRENCY", 1),

		ExtractCommitments: boolean("ANAMNESIA_EXTRACT_COMMITMENTS", false),

		ExtractGraph: boolean("ANAMNESIA_GRAPH_EXTRACT", false),
		GraphMaxOps:  num("ANAMNESIA_GRAPH_MAX_OPS", 12),

		GraphCandidateDistance: fraction("ANAMNESIA_GRAPH_CANDIDATE_DISTANCE", 0.45),

		DecayHalfLifeCase:     dur("ANAMNESIA_DECAY_HALF_LIFE_CASE", 336*time.Hour),
		DecayHalfLifeStrategy: dur("ANAMNESIA_DECAY_HALF_LIFE_STRATEGY", 8760*time.Hour),
		DecayHalfLifeHybrid:   dur("ANAMNESIA_DECAY_HALF_LIFE_HYBRID", 1440*time.Hour),

		ActivityEnabled: boolean("ANAMNESIA_ACTIVITY_ENABLED", true),
		ActivityTraces:  num("ANAMNESIA_ACTIVITY_TRACES", 200),
	}

	if strings.TrimSpace(c.DatabaseURL) == "" {
		fail("ANAMNESIA_DATABASE_URL is required")
	}

	// Each provider needs its key. Checked here so the failure names the
	// missing variable instead of surfacing as a 401 during a background
	// worker tick hours later.
	for _, need := range []struct {
		when   bool
		envVar string
		why    string
	}{
		{c.EmbedProvider == "openai", "OPENAI_API_KEY", "embed.provider=openai"},
		{c.EmbedProvider == "openrouter", "OPENROUTER_API_KEY", "embed.provider=openrouter"},
		{c.LLMProvider == "anthropic", "ANTHROPIC_API_KEY", "llm.provider=anthropic"},
		{c.LLMProvider == "openai", "OPENAI_API_KEY", "llm.provider=openai"},
		{c.LLMProvider == "openrouter", "OPENROUTER_API_KEY", "llm.provider=openrouter"},
		{c.RerankProvider == "cohere", "COHERE_API_KEY", "rerank.provider=cohere"},
		{c.RerankProvider == "openrouter", "OPENROUTER_API_KEY", "rerank.provider=openrouter"},
		{c.PIIProvider == "presidio", "ANAMNESIA_PRESIDIO_URL", "pii.provider=presidio"},
	} {
		if need.when && strings.TrimSpace(os.Getenv(need.envVar)) == "" {
			fail("%s is required when %s", need.envVar, need.why)
		}
	}

	if len(errs) > 0 {
		return nil, errors.New("configuration: " + strings.Join(errs, "; "))
	}
	return c, nil
}

// providerDefault picks the default provider value. Explicit settings
// (anything non-empty, including "stub" / "none") win unconditionally;
// when nothing is set, return ifOR if the auto-light-up condition holds
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

// defaultLLMModel picks a sensible default per provider so the user only
// needs to set the provider and key to get going.
func defaultLLMModel(provider string) string {
	switch provider {
	case "anthropic":
		return "claude-sonnet-4-6"
	case "openai":
		return "gpt-4o-mini"
	case "openrouter":
		// OpenRouter slugs use dot-separated versions, unlike Anthropic's
		// dash-separated direct-API names.
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

// String returns a redacted single-line summary suitable for logs.
func (c *Config) String() string {
	return fmt.Sprintf(
		"http=%s db=%s embed=%s/%s/%d llm=%s/%s rerank=%s",
		c.HTTPAddr, redact(c.DatabaseURL),
		c.EmbedProvider, c.EmbedModel, c.EmbedDims,
		c.LLMProvider, c.LLMModel, c.RerankProvider,
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
