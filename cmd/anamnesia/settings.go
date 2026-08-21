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
	"math"
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
	// kFraction is a number from 0 to 1 inclusive. The bound is part of
	// the kind rather than per-setting, because it is the bound
	// internal/config's fraction() already enforces when the server
	// reads the value back: a kind that accepted more here would put
	// the two ends of the path back into disagreement, which is the
	// whole reason this kind exists.
	kFraction
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
	// Project marks a setting worth overriding for one repository, and
	// so worth rendering into the .anamnesia.toml `anamnesia init`
	// writes. That file is committed with the repository, which is why a
	// kSecret setting can never be one of these.
	Project bool
	// Zeroable lets a numeric setting be set to 0, where 0 means "off"
	// rather than "unset". Most numeric settings are sizes or intervals
	// that cannot be zero, so this is opt-in per setting.
	Zeroable bool
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
	{Key: "identity.user", Kind: kString, Def: "", Env: "ANAMNESIA_DEFAULT_USER", Project: true,
		Doc: "Who memories belong to. Defaults to your OS username. A small team can share one server by giving each person a distinct handle."},
	{Key: "identity.project", Kind: kString, Def: "", Env: "", Project: true,
		Doc: "Project slug memories are filed under. Defaults to the git repository's directory name. Belongs in a repository's ./.anamnesia.toml, which `anamnesia init` writes."},

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
	{Key: "server.url", Kind: kString, Def: "", Env: "", Project: true,
		Doc: "Where the CLI and hooks look for the server. Leave empty to derive it from server.addr. Set it to point at a server on another machine."},
	{Key: "server.token", Kind: kSecret, Env: "ANAMNESIA_SERVER_TOKEN",
		Doc: "Optional shared secret. Required in practice whenever server.addr is not loopback."},
	{Key: "server.autostart", Kind: kBool, Def: "true", Env: "",
		Doc: "Let hooks start the stack on demand when it is not running, so a new session heals itself instead of silently losing memory."},
	{Key: "server.shutdown_wait", Kind: kDuration, Def: "30s", Env: "ANAMNESIA_SHUTDOWN_WAIT",
		Doc: "How long the server may take to finish in-flight work when asked to stop. `stop` and `restart` wait this out before reporting a server that will not exit."},

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
	{Key: "worker.consolidate_similarity", Kind: kFraction, Def: "0.65", Env: "ANAMNESIA_CONSOLIDATE_SIMILARITY",
		Doc: "How alike two experiences must be, as a cosine from 0 to 1, before consolidation folds them into one insight. This shipped hardcoded at 0.85, which no real corpus reaches: measured over the 1,402 same-scope pairs on a live install, the mean was 0.289 and the single most similar pair scored 0.754, so nothing ever clustered and every pass reported success while folding nothing. 0.65 was chosen by replaying the clusterer over that corpus and reading what it merged — it forms clean topical pairs. Lower it to 0.60 to fold whole threads rather than pairs, at the risk of a summary that blurs its sources and then competes with them in retrieval."},
	{Key: "worker.consolidate_max_cluster", Kind: kInt, Def: "8", Env: "ANAMNESIA_CONSOLIDATE_MAX_CLUSTER",
		Doc: "Most experiences one insight may be distilled from. A cluster that fills up does not spill: the next similar experience opens a second cluster, so a long-running thread becomes several summaries rather than one incoherent one. Raise it if your threads are longer than eight sessions and you would rather have one summary than several."},
	{Key: "worker.extract_concurrency", Kind: kInt, Def: "1", Env: "ANAMNESIA_EXTRACT_CONCURRENCY",
		Doc: "How many sources the extractor works on at once. Extraction is mostly waiting on the model, so raising this is close to a linear speedup and does not cost more tokens. It does change what is extracted: sources handled together stop seeing each other's facts as merge candidates, so a bulk backfill of related sessions can produce duplicates a serial drain would have merged. Raise it for benchmarks and backfills; leave it at 1 for a live install where dedup matters more than throughput."},
	{Key: "worker.extract_commitments", Kind: kBool, Def: "false", Env: "ANAMNESIA_EXTRACT_COMMITMENTS",
		Doc: "Also record open obligations (\"I'll send X by Friday\") in the commitments ledger."},

	// ─── ingest ──────────────────────────────────────────────────────
	{Key: "ingest.flush_bytes", Kind: kInt, Def: "16384", Env: "", Zeroable: true,
		Doc: "Checkpoint mid-session once this many new bytes of transcript have accumulated. Stop fires after every assistant turn; this decides when that turn is worth a checkpoint. Bytes rather than turns because it is what lines up with segments: reaching it means there is a segment's worth of new material to cut, so a flush produces whole segments instead of slivers. Because checkpoints are incremental, flushing often costs about the same as flushing once at the end — the same bytes, cut the same way — it just stops the work waiting for the session to finish. Set to 0 to use only the time gate."},
	{Key: "ingest.flush_after", Kind: kDuration, Def: "20m", Env: "", Zeroable: true,
		Doc: "Checkpoint mid-session once this long has passed since the last one, however little has accumulated. The backstop for a slow conversation that never reaches ingest.flush_bytes quickly but should still not sit uncheckpointed for hours. Set to 0 to use only the byte gate; set both to 0 to checkpoint only at PreCompact and SessionEnd, which is what earlier versions did."},
	{Key: "ingest.recover_stranded", Kind: kBool, Def: "true", Env: "",
		Doc: "Ingest transcript tails from sessions that ended without a checkpoint. A checkpoint fires on PreCompact and SessionEnd, so a session that crashes or is killed never sends its last stretch of work. The transcript is still on disk and the offset file records how far it was read, so `anamnesia recover` reads the rest; session start runs it in the background. Turn it off to leave abandoned tails alone."},
	{Key: "ingest.recover_idle", Kind: kDuration, Def: "15m", Env: "",
		Doc: "How long a transcript must go unwritten before recovery treats its session as over. This is the only judgement recovery makes, and it cuts both ways: too short and it ingests a live session's tail, racing that session's own checkpoint and paying to extract content that is about to be sent again; too long and a crashed session's work sits uncollected for longer. Nothing is lost either way, since the transcript stays on disk until recovery reads it."},
	{Key: "ingest.segment_gap", Kind: kDuration, Def: "20m", Env: "", Zeroable: true,
		Doc: "A pause longer than this starts a new segment when a checkpoint is cut up, so the surprise gate judges one subject at a time rather than a whole session. Set to 0 to send each checkpoint as a single source, which is what earlier versions did."},
	{Key: "ingest.segment_max_bytes", Kind: kInt, Def: "4000", Env: "", Zeroable: true,
		Doc: "A segment is cut when it grows past this, because a long unbroken session is still not one idea. It is also what bounds how much the extractor has to hold at once: attention degrades over a long input, so a bigger segment does not mean more extracted, it means less. Measured on three real 21KB sessions, the same content yielded 14 unique facts at 32768 and 74 at 4000 — and the ones only the smaller cap found were standing preferences like \"branch instead of committing directly to main\", exactly what memory is for. The cost is one model call per segment, so 3 calls became 18. Raise it to spend less, set to 0 to disable the size cut."},

	// ─── decay ───────────────────────────────────────────────────────
	{Key: "decay.half_life_case", Kind: kDuration, Def: "336h", Env: "ANAMNESIA_DECAY_HALF_LIFE_CASE",
		Doc: "How long a remembered episode takes to lose half its relevance. Two weeks by default: what you did last fortnight matters, what you did last spring usually does not. Recomputed every worker.decay_every."},
	{Key: "decay.half_life_strategy", Kind: kDuration, Def: "8760h", Env: "ANAMNESIA_DECAY_HALF_LIFE_STRATEGY",
		Doc: "The same for a learned approach rather than an episode. A year by default, which is close enough to never: how you solve a problem outlives the day you solved it."},
	{Key: "decay.half_life_hybrid", Kind: kDuration, Def: "1440h", Env: "ANAMNESIA_DECAY_HALF_LIFE_HYBRID",
		Doc: "The same for an experience that is part episode, part approach. Two months by default, between the other two."},

	// ─── activity ────────────────────────────────────────────────────
	{Key: "activity.enabled", Kind: kBool, Def: "true", Env: "ANAMNESIA_ACTIVITY_ENABLED",
		Doc: "Record what the server is doing, in memory, and serve it on /v1/activity. Off makes those routes 404 and every recording call a no-op."},
	{Key: "activity.traces", Kind: kInt, Def: "200", Env: "ANAMNESIA_ACTIVITY_TRACES",
		Doc: "How many recent traces to keep. They live in memory only, so a restart clears them. Use activity.enabled to switch recording off; this is a size, and sizes are positive."},

	// ─── graph ───────────────────────────────────────────────────────
	{Key: "graph.extract", Kind: kBool, Def: "false", Env: "ANAMNESIA_GRAPH_EXTRACT",
		Doc: "Extract entities and relationships from a session, in one extra model call per checkpoint. Off by default: it costs a call, and an install that never reads the graph should not pay for it."},
	{Key: "graph.max_ops", Kind: kInt, Def: "12", Env: "ANAMNESIA_GRAPH_MAX_OPS",
		Doc: "Caps how many entities and edges one checkpoint may produce."},
	{Key: "graph.candidate_distance", Kind: kFraction, Def: "0.45", Env: "ANAMNESIA_GRAPH_CANDIDATE_DISTANCE",
		Doc: "How close an existing entity's name must embed to a newly extracted entity's name, and share its kind, before it is offered to the model as a possible match — triggering one extra, otherwise-skipped model call per checkpoint to ask whether the two are really the same thing. Cosine distance from 0 to 1, so smaller is stricter. This does not merge anything by itself: the model decides identity, so it is deliberately loose; raise it only if relevant entities are consistently missing from what the model is offered. Past 1 the two names point away from each other, which is not a candidate for being the same thing, so 1 is the ceiling."},
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

// valueCompletions returns the values this setting accepts, for shell
// completion. Only kinds with a closed set have one: offering a guess for
// a model name or an API key would be worse than offering nothing.
func (s setting) valueCompletions() []string {
	switch s.Kind {
	case kEnum:
		out := make([]string, 0, len(s.Values))
		for _, v := range s.Values {
			// The empty value means "unset". There is nothing to type.
			if v != "" {
				out = append(out, v)
			}
		}
		return out
	case kBool:
		return []string{"true", "false"}
	}
	return nil
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
		if n < 0 || (n == 0 && !s.Zeroable) {
			return "", fmt.Errorf("%s must be positive, got %d", s.Key, n)
		}
		return strconv.Itoa(n), nil
	case kFraction:
		if v == "" {
			return "", nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return "", fmt.Errorf("%s must be a number between 0 and 1, got %q", s.Key, raw)
		}
		// NaN compares false to everything, so the range check below
		// lets it through. A NaN threshold makes every `distance <=
		// threshold` test false, which silently turns whatever reads
		// this setting into a no-op that still traces as healthy.
		if math.IsNaN(f) || f < 0 || f > 1 {
			return "", fmt.Errorf("%s must be between 0 and 1, got %q", s.Key, raw)
		}
		return v, nil
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
		if d < 0 || (d == 0 && !s.Zeroable) {
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
