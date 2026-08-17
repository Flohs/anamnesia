// settings.go is the single definition of every Anamnesia setting.
//
// One table drives all of it: the generated config.toml and its comments,
// validation for `anamnesia config set`, the output of `anamnesia config
// list`, and the environment handed to the server process. Adding a
// setting in one place therefore cannot drift from its documentation, its
// default, or the server that consumes it — which is what previously let
// docker-compose.yml, .env.example and internal/config disagree with each
// other about what existed and what the defaults were.
package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// kind is how a value is validated and rendered.
type kind int

const (
	kString kind = iota
	kInt
	kBool
	kDuration
	kSecret // string, but masked in output
	kEnum
)

// setting describes one dotted configuration key.
type setting struct {
	// Key is the dotted name used on the command line and in the file,
	// e.g. "openrouter.api_key" for [openrouter] api_key.
	Key  string
	Kind kind
	Def  string
	Doc  string
	// Env is the server environment variable this maps to. Empty means the
	// setting is host-side only (container management, hook behaviour) and
	// is never handed to the server.
	Env string
	// Values enumerates the accepted values for kEnum.
	Values []string
	// Generated marks values created at setup time (the database
	// password), so regenerating a config does not overwrite them.
	Generated bool
}

func (s setting) section() string { return strings.SplitN(s.Key, ".", 2)[0] }
func (s setting) name() string {
	parts := strings.SplitN(s.Key, ".", 2)
	if len(parts) != 2 {
		return s.Key
	}
	return parts[1]
}

// settings is the complete, ordered list. Order controls the layout of the
// generated config file, so it reads top-down from "what you must touch"
// to "what you almost never touch".
var settings = []setting{
	// ─── identity ────────────────────────────────────────────────────
	{Key: "identity.user", Kind: kString, Def: "", Env: "ANAMNESIA_DEFAULT_USER",
		Doc: "Who memories belong to. Defaults to your OS username. A small team can share one server by giving each person a distinct handle."},
	{Key: "identity.project", Kind: kString, Def: "", Env: "",
		Doc: "Project slug memories are filed under. Defaults to the git repository's directory name. Usually set per project in ./.anamnesia.toml rather than here."},

	// ─── providers ───────────────────────────────────────────────────
	{Key: "openrouter.api_key", Kind: kSecret, Env: "OPENROUTER_API_KEY",
		Doc: "One key fronts chat, embeddings and rerank. Setting it switches all three providers to openrouter unless you pick them explicitly. Get one at https://openrouter.ai/keys."},
	{Key: "openai.api_key", Kind: kSecret, Env: "OPENAI_API_KEY",
		Doc: "Needed for llm.provider=openai or embed.provider=openai."},
	{Key: "openai.base_url", Kind: kString, Def: "https://api.openai.com/v1", Env: "OPENAI_BASE_URL",
		Doc: "Point this at vLLM, Ollama or Azure to use an OpenAI-compatible endpoint instead."},
	{Key: "anthropic.api_key", Kind: kSecret, Env: "ANTHROPIC_API_KEY",
		Doc: "Needed for llm.provider=anthropic."},
	{Key: "cohere.api_key", Kind: kSecret, Env: "COHERE_API_KEY",
		Doc: "Needed for rerank.provider=cohere."},

	// ─── llm ─────────────────────────────────────────────────────────
	{Key: "llm.provider", Kind: kEnum, Def: "", Env: "ANAMNESIA_LLM_PROVIDER",
		Values: []string{"", "stub", "anthropic", "openai", "openrouter"},
		Doc:    "Extraction and consolidation model. Leave empty to auto-pick: openrouter when openrouter.api_key is set, otherwise stub. The stub extracts nothing, which is fine for trying things out."},
	{Key: "llm.model", Kind: kString, Def: "", Env: "ANAMNESIA_LLM_MODEL",
		Doc: "Leave empty for the provider default (anthropic/claude-sonnet-4.6 on openrouter, claude-sonnet-4-6 direct, gpt-4o-mini on openai)."},
	{Key: "llm.timeout", Kind: kDuration, Def: "120s", Env: "ANAMNESIA_LLM_HTTP_TIMEOUT",
		Doc: "Per-request HTTP timeout. Raise it a lot for a cold local model."},

	// ─── embeddings ──────────────────────────────────────────────────
	{Key: "embed.provider", Kind: kEnum, Def: "", Env: "ANAMNESIA_EMBED_PROVIDER",
		Values: []string{"", "stub", "openai", "openrouter"},
		Doc:    "Vector embeddings for retrieval. Same auto-pick rule as llm.provider."},
	{Key: "embed.model", Kind: kString, Def: "", Env: "ANAMNESIA_EMBED_MODEL",
		Doc: "Leave empty for the provider default (text-embedding-3-small)."},
	{Key: "embed.dims", Kind: kInt, Def: "1536", Env: "ANAMNESIA_EMBED_DIMS",
		Doc: "Embedding width. Must match your model: 1536 for text-embedding-3-small, 3072 for -3-large, 768 for nomic-embed-text. Changing this needs `anamnesia migrate --dims N`, which rebuilds the columns and discards existing vectors so they can be re-embedded."},

	// ─── rerank ──────────────────────────────────────────────────────
	{Key: "rerank.provider", Kind: kEnum, Def: "", Env: "ANAMNESIA_RERANK_PROVIDER",
		Values: []string{"", "none", "cohere", "openrouter"},
		Doc:    "Optional second-pass scoring of search candidates. Costs latency, buys precision."},
	{Key: "rerank.model", Kind: kString, Def: "", Env: "ANAMNESIA_RERANK_MODEL",
		Doc: "Leave empty for the provider default (cohere/rerank-v3.5 on openrouter)."},

	// ─── server ──────────────────────────────────────────────────────
	{Key: "server.addr", Kind: kString, Def: "127.0.0.1:8181", Env: "ANAMNESIA_HTTP_ADDR",
		Doc: "Address the local server listens on. Loopback by default; anything else exposes your memory to the network, so set server.token too."},
	{Key: "server.url", Kind: kString, Def: "", Env: "",
		Doc: "Where the CLI and hooks look for the server. Leave empty to derive it from server.addr. Set it to point at a server on another machine."},
	{Key: "server.token", Kind: kSecret, Env: "ANAMNESIA_SERVER_TOKEN",
		Doc: "Optional shared secret. Required in practice whenever server.addr is not loopback."},
	{Key: "server.autostart", Kind: kBool, Def: "true", Env: "",
		Doc: "Let hooks start the stack on demand when it is not running, so a new session heals itself instead of silently losing memory."},

	// ─── postgres ────────────────────────────────────────────────────
	{Key: "postgres.url", Kind: kString, Def: "", Env: "",
		Doc: "Use an existing Postgres instead of a managed container. Needs the pgvector extension available. When set, every other postgres.* setting is ignored and Anamnesia manages no container."},
	{Key: "postgres.image", Kind: kString, Def: "pgvector/pgvector:pg16", Env: "",
		Doc: "Image for the managed container."},
	{Key: "postgres.container", Kind: kString, Def: "anamnesia-postgres", Env: "",
		Doc: "Container name. Anamnesia only ever touches a container with this exact name."},
	{Key: "postgres.volume", Kind: kString, Def: "anamnesia-pgdata", Env: "",
		Doc: "Docker volume holding the data. Your memory lives here; removing it deletes everything."},
	{Key: "postgres.port", Kind: kInt, Def: "5434", Env: "",
		Doc: "Host port for the container, bound to loopback only. Change it if something already uses 5434."},
	{Key: "postgres.user", Kind: kString, Def: "anamnesia", Env: ""},
	{Key: "postgres.password", Kind: kSecret, Def: "", Env: "", Generated: true,
		Doc: "Generated at setup. Only reachable from this machine."},
	{Key: "postgres.database", Kind: kString, Def: "anamnesia", Env: ""},

	// ─── pii ─────────────────────────────────────────────────────────
	{Key: "pii.provider", Kind: kEnum, Def: "regex", Env: "ANAMNESIA_PII_PROVIDER",
		Values: []string{"", "none", "regex", "presidio"},
		Doc:    "Detects personal data before anything is stored. regex is in-process; presidio calls a sidecar."},
	{Key: "pii.mode", Kind: kEnum, Def: "tag", Env: "ANAMNESIA_PII_MODE",
		Values: []string{"tag", "redact"},
		Doc:    "tag records which categories were found; redact also replaces the matches before storing."},
	{Key: "pii.presidio_url", Kind: kString, Def: "", Env: "ANAMNESIA_PRESIDIO_URL",
		Doc: "Required when pii.provider=presidio."},

	// ─── worker ──────────────────────────────────────────────────────
	{Key: "worker.extract_every", Kind: kDuration, Def: "15s", Env: "ANAMNESIA_EXTRACT_EVERY",
		Doc: "How often the extractor drains newly ingested sources."},
	{Key: "worker.embed_backfill", Kind: kDuration, Def: "1m", Env: "ANAMNESIA_EMBED_BACKFILL",
		Doc: "How often rows missing a vector get embedded."},
	{Key: "worker.forget_every", Kind: kDuration, Def: "1h", Env: "ANAMNESIA_FORGET_EVERY",
		Doc: "How often expired working memory is purged."},
	{Key: "worker.decay_every", Kind: kDuration, Def: "1h", Env: "ANAMNESIA_DECAY_EVERY",
		Doc: "How often experience relevance is recomputed."},
	{Key: "worker.consolidate_every", Kind: kDuration, Def: "24h", Env: "ANAMNESIA_CONSOLIDATE_EVERY",
		Doc: "How often similar experiences are clustered and distilled."},
	{Key: "worker.extract_commitments", Kind: kBool, Def: "false", Env: "ANAMNESIA_EXTRACT_COMMITMENTS",
		Doc: "Also record open obligations (\"I'll send X by Friday\") in the commitments ledger."},
}

// settingByKey indexes settings for lookup.
var settingByKey = func() map[string]setting {
	m := make(map[string]setting, len(settings))
	for _, s := range settings {
		m[s.Key] = s
	}
	return m
}()

// sectionOrder is the section list in file order, de-duplicated.
func sectionOrder() []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range settings {
		if sec := s.section(); !seen[sec] {
			seen[sec] = true
			out = append(out, sec)
		}
	}
	return out
}

// knownKeys returns every key, sorted, for error messages.
func knownKeys() []string {
	out := make([]string, 0, len(settings))
	for _, s := range settings {
		out = append(out, s.Key)
	}
	sort.Strings(out)
	return out
}

// validate checks a raw string against the setting's kind, returning the
// normalised form. This is the gate that stops a typo becoming a silent
// fallback to the default hours later.
func (s setting) validate(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	switch s.Kind {
	case kString, kSecret:
		return v, nil
	case kInt:
		if v == "" {
			return "", nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return "", fmt.Errorf("%s must be a number, got %q", s.Key, raw)
		}
		if n <= 0 {
			return "", fmt.Errorf("%s must be positive, got %d", s.Key, n)
		}
		return strconv.Itoa(n), nil
	case kBool:
		if v == "" {
			return "", nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return "", fmt.Errorf("%s must be true or false, got %q", s.Key, raw)
		}
		return strconv.FormatBool(b), nil
	case kDuration:
		if v == "" {
			return "", nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return "", fmt.Errorf("%s must be a duration like 15s, 1m or 24h, got %q", s.Key, raw)
		}
		if d <= 0 {
			return "", fmt.Errorf("%s must be positive, got %q", s.Key, raw)
		}
		return v, nil
	case kEnum:
		for _, ok := range s.Values {
			if v == ok {
				return v, nil
			}
		}
		var shown []string
		for _, ok := range s.Values {
			if ok == "" {
				shown = append(shown, "(empty)")
			} else {
				shown = append(shown, ok)
			}
		}
		return "", fmt.Errorf("%s must be one of %s, got %q", s.Key, strings.Join(shown, ", "), raw)
	}
	return v, nil
}

// mask renders a value for display, hiding secrets.
func (s setting) mask(v string) string {
	if s.Kind != kSecret || v == "" {
		return v
	}
	if len(v) <= 4 {
		return "••••"
	}
	return "••••" + v[len(v)-4:]
}
