// serve.go runs the server, and applies migrations.
//
// `anamnesia start` spawns this as a detached background process; it is the
// same binary, so there is no separate server artifact to install, version
// or keep in sync.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/config"
	"github.com/flohs/anamnesia/internal/decay"
	"github.com/flohs/anamnesia/internal/embed"
	"github.com/flohs/anamnesia/internal/extract"
	"github.com/flohs/anamnesia/internal/httpapi"
	"github.com/flohs/anamnesia/internal/jobs"
	"github.com/flohs/anamnesia/internal/llm"
	"github.com/flohs/anamnesia/internal/mcp"
	"github.com/flohs/anamnesia/internal/pii"
	"github.com/flohs/anamnesia/internal/retrieval"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

var (
	serveWorkerOnly bool
	serveNoWorker   bool
	migrateDims     int
)

var serveCmd = &cobra.Command{
	Use:    "serve",
	Short:  "Run the HTTP API, MCP endpoint and background worker",
	Hidden: true,
	Long: "Run the Anamnesia server: HTTP API on /v1/*, MCP transport on /mcp,\n" +
		"and the background workers. Normally started for you by\n" +
		"`anamnesia start`, which runs it detached and captures its log.",
	RunE: runServe,
}

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply database migrations and exit",
	Long: "Apply any pending migrations.\n\n" +
		"With --dims, also rebuild the embedding columns and their ANN indexes\n" +
		"for a different vector width. That discards stored vectors, because a\n" +
		"vector cannot be reinterpreted at another width and one produced by a\n" +
		"different model is not comparable anyway. The embed worker re-embeds\n" +
		"everything on its next tick.",
	RunE: runMigrate,
}

func init() {
	serveCmd.Flags().BoolVar(&serveWorkerOnly, "worker", false, "run only the background worker")
	serveCmd.Flags().BoolVar(&serveNoWorker, "no-worker", false, "skip the background worker (HTTP only)")
	migrateCmd.Flags().IntVar(&migrateDims, "dims", 0, "rebuild the embedding columns at this width")
}

// applyHostEnv projects ~/.anamnesia/config.toml into this process's
// environment so config.Load() sees it.
//
// `anamnesia start` already hands the server this environment, but running
// `anamnesia serve` or `anamnesia migrate` by hand has to work too, and
// requiring the user to export a dozen variables to do so would make the
// config file a lie.
func applyHostEnv() (*hostConfig, error) {
	hc, err := loadHostConfig()
	if err != nil {
		return nil, err
	}
	for _, kv := range hc.ServerEnv() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			return nil, err
		}
	}
	return hc, nil
}

// openStore dials Postgres and applies migrations.
func openStore(ctx context.Context, cfg *config.Config) (*store.Store, error) {
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// configSnapshot renders the resolved settings for /v1/config. Secrets
// are masked here, on the host side: settings live in this package and
// internal/httpapi cannot import it, so the HTTP layer is handed data it
// can serve rather than a configuration it could read a key out of.
func configSnapshot(hc *hostConfig) []httpapi.ConfigItem {
	items := make([]httpapi.ConfigItem, 0, len(settings))
	for _, s := range settings {
		items = append(items, httpapi.ConfigItem{
			Key:    s.Key,
			Value:  s.mask(hc.Get(s.Key)),
			Source: string(hc.Origin(s.Key)),
			Secret: s.Kind == kSecret,
		})
	}
	return items
}

// decayConfig turns the configured half-lives into the worker's policy.
//
// Until these became settings the worker fell back to its own defaults,
// so nothing here was configurable and /v1/config could not report what
// the running server was actually forgetting by.
func decayConfig(cfg *config.Config) decay.Config {
	return decay.Config{
		HalfLives: map[anamnesia.ExperienceKind]time.Duration{
			anamnesia.ExperienceCase:     cfg.DecayHalfLifeCase,
			anamnesia.ExperienceStrategy: cfg.DecayHalfLifeStrategy,
			anamnesia.ExperienceHybrid:   cfg.DecayHalfLifeHybrid,
		},
	}
}

// extractConfig turns the resolved configuration into the extractor's
// own. It is the last hop of the path a setting takes — file, resolution,
// server environment, config.Load, here — and the only one that was
// invisible to a test while it was a struct literal inside runServe.
func extractConfig(cfg *config.Config) extract.Config {
	return extract.Config{
		ExtractCommitments:     cfg.ExtractCommitments,
		ExtractGraph:           cfg.ExtractGraph,
		GraphMaxOps:            cfg.GraphMaxOps,
		GraphCandidateDistance: cfg.GraphCandidateDistance,
	}
}

func runServe(cmd *cobra.Command, _ []string) error {
	hc, err := applyHostEnv()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Best effort: without a resolvable home there is no hook log to
	// serve, and /v1/hooks says so rather than the server refusing to
	// start over it.
	hookLog, _ := hookLogPath()
	log := rf.log
	log.Info("anamnesia booting", "cfg", cfg.String(), "version", version)

	ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	// Refuse to serve into a schema that cannot hold what the embedder
	// produces. Every embedding write would fail, one at a time, in a
	// background worker nobody is watching.
	schemaDims, err := st.EmbeddingDims(ctx)
	if err != nil {
		return err
	}
	if schemaDims != cfg.EmbedDims {
		return fmt.Errorf(
			"schema stores vector(%d) but embed.dims is %d, so every embedding write would fail.\n"+
				"Run `anamnesia migrate --dims %d` to rebuild the schema, or set embed.dims to %d",
			schemaDims, cfg.EmbedDims, cfg.EmbedDims, schemaDims)
	}
	if missing, err := st.MissingANNIndexes(ctx); err == nil && len(missing) > 0 {
		if store.ANNIndexableDims(schemaDims) {
			log.Warn("no ANN index on embedding columns, vector search will be a sequential scan",
				"tables", missing, "fix", fmt.Sprintf("anamnesia migrate --dims %d", schemaDims))
		} else {
			log.Warn("embedding width exceeds pgvector's HNSW limit, vector search is a sequential scan",
				"dims", schemaDims, "limit", 2000)
		}
	}

	embKey := cfg.OpenAIAPIKey
	if cfg.EmbedProvider == "openrouter" {
		embKey = cfg.OpenRouterAPIKey
	}
	emb, err := embed.New(cfg.EmbedProvider, cfg.EmbedModel, cfg.OpenAIBaseURL, embKey, cfg.EmbedDims)
	if err != nil {
		return err
	}
	llmKey := cfg.AnthropicAPIKey
	switch cfg.LLMProvider {
	case "openai":
		llmKey = cfg.OpenAIAPIKey
	case "openrouter":
		llmKey = cfg.OpenRouterAPIKey
	}
	llmc, err := llm.New(llm.Config{
		Provider: cfg.LLMProvider,
		Model:    cfg.LLMModel,
		APIKey:   llmKey,
		BaseURL:  cfg.OpenAIBaseURL,
		Timeout:  cfg.LLMHTTPTimeout,
	})
	if err != nil {
		return err
	}
	rerankKey := cfg.CohereAPIKey
	if cfg.RerankProvider == "openrouter" {
		rerankKey = cfg.OpenRouterAPIKey
	}
	reranker, err := retrieval.NewReranker(cfg.RerankProvider, rerankKey, cfg.RerankModel)
	if err != nil {
		return err
	}
	piiDet, err := pii.New(cfg.PIIProvider, cfg.PresidioURL, pii.Mode(cfg.PIIMode))
	if err != nil {
		return err
	}

	// The recorder is what /v1/activity serves. Nil when recording is
	// switched off, which every call site tolerates and which makes the
	// activity routes 404 rather than pretend to be empty.
	var recorder *activity.Recorder
	if cfg.ActivityEnabled {
		recorder = activity.New(cfg.ActivityTraces)
	}

	retr := &retrieval.Engine{Store: st, Embedder: emb, Reranker: reranker, Log: log}
	briefer := &jobs.Briefer{LLM: llmc}

	mcpHandler := mcp.NewHandler(mcp.Deps{
		Store:          st,
		Retrieval:      retr,
		Embedder:       emb,
		PII:            piiDet,
		Briefer:        briefer,
		DefaultUser:    cfg.DefaultUser,
		DefaultProject: cfg.DefaultProject,
	})

	srv := httpapi.NewServer(cfg.HTTPAddr, httpapi.Deps{
		Store:          st,
		Retrieval:      retr,
		MCPHandler:     mcpHandler,
		PII:            piiDet,
		Briefer:        briefer,
		DefaultUser:    cfg.DefaultUser,
		DefaultProject: cfg.DefaultProject,
		ServerToken:    cfg.ServerToken,
		Log:            log,
		Version:        version,
		Activity:       recorder,
		Started:        time.Now().UTC(),
		Config:         configSnapshot(hc),
		HookLogPath:    hookLog,
		EmbedProvider:  cfg.EmbedProvider,
		EmbedModel:     cfg.EmbedModel,
		EmbedDims:      cfg.EmbedDims,
		LLMProvider:    cfg.LLMProvider,
		LLMModel:       cfg.LLMModel,
	})

	var workerErr chan error
	if !serveNoWorker {
		workerErr = make(chan error, 1)
		worker := &jobs.Worker{
			Cfg: jobs.Config{
				EmbedEvery:         cfg.EmbedBackfill,
				ForgetEvery:        cfg.ForgetEvery,
				DecayEvery:         cfg.DecayEvery,
				ConsolidateEvery:   cfg.ConsolidateEvery,
				ExtractEvery:       cfg.ExtractEvery,
				ExtractConcurrency: cfg.ExtractConcurrency,
				Extract:            extractConfig(cfg),
				Decay:              decayConfig(cfg),
			},
			Store:     st,
			Embedder:  emb,
			LLM:       llmc,
			Retrieval: retr,
			Log:       log,
			Activity:  recorder,
		}
		go func() { workerErr <- worker.Run(ctx) }()
	}

	if serveWorkerOnly {
		log.Info("worker-only mode")
		<-ctx.Done()
		if workerErr != nil {
			<-workerErr
		}
		return nil
	}

	httpErr := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", cfg.HTTPAddr)
		err := srv.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown requested")
	case err := <-httpErr:
		log.Error("http server crashed", "err", err)
		return err
	}

	shCtx, shCancel := context.WithTimeout(context.Background(), cfg.ShutdownWait)
	defer shCancel()
	if err := srv.Shutdown(shCtx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	if workerErr != nil {
		select {
		case <-workerErr:
		case <-time.After(cfg.ShutdownWait):
		}
	}
	return nil
}

func runMigrate(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	hc, err := applyHostEnv()
	if err != nil {
		return err
	}
	// Migrating a database that is not running is a confusing dial error,
	// so bring the container up first.
	if err := ensurePostgres(ctx, hc, out); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	v, err := st.MigrationVersion(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "✦ schema at version %d\n", v)

	dims := migrateDims
	if dims == 0 {
		// Without an explicit width, still reconcile the schema with the
		// configured one. This is what repairs an install whose columns and
		// configuration disagree, and what restores ANN indexes.
		dims = cfg.EmbedDims
	}
	current, err := st.EmbeddingDims(ctx)
	if err != nil {
		return err
	}
	missing, err := st.MissingANNIndexes(ctx)
	if err != nil {
		return err
	}
	if current == dims && len(missing) == 0 {
		fmt.Fprintf(out, "✦ embedding columns are vector(%d) with ANN indexes in place\n", dims)
		return nil
	}
	if current != dims {
		fmt.Fprintf(out, "✦ rebuilding embedding columns: vector(%d) → vector(%d)\n", current, dims)
		fmt.Fprintln(out, "  stored vectors are discarded and will be re-embedded in the background")
	} else {
		fmt.Fprintf(out, "✦ rebuilding missing ANN indexes on %v\n", missing)
	}
	if err := st.SetEmbeddingDims(ctx, dims); err != nil {
		return err
	}
	if !store.ANNIndexableDims(dims) {
		fmt.Fprintf(out, "  note: %d dimensions exceeds pgvector's HNSW limit of 2000, so no ANN index was created and vector search will scan\n", dims)
	}
	fmt.Fprintf(out, "✦ embedding columns are now vector(%d)\n", dims)
	if dims != cfg.EmbedDims {
		fmt.Fprintf(out, "  remember to set embed.dims to %d: anamnesia config set embed.dims %d\n", dims, dims)
	}
	return nil
}
