package main

import (
	"context"
	"errors"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/flohs/anamnesia-open-source/internal/config"
	"github.com/flohs/anamnesia-open-source/internal/embed"
	"github.com/flohs/anamnesia-open-source/internal/httpapi"
	"github.com/flohs/anamnesia-open-source/internal/jobs"
	"github.com/flohs/anamnesia-open-source/internal/llm"
	"github.com/flohs/anamnesia-open-source/internal/mcp"
	"github.com/flohs/anamnesia-open-source/internal/pii"
	"github.com/flohs/anamnesia-open-source/internal/retrieval"
	"github.com/flohs/anamnesia-open-source/internal/store"
)

var (
	serveWorkerOnly bool
	serveNoWorker   bool
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the HTTP API + MCP + worker (inside docker-compose)",
	Long: "Run the Anamnesia server: HTTP API on /v1/*, MCP transport on /mcp,\n" +
		"and the background worker (embed backfill, working-memory expiry).\n" +
		"All three run in-process by default.",
	RunE: runServe,
}

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply database migrations and exit",
	RunE:  runMigrate,
}

func init() {
	serveCmd.Flags().BoolVar(&serveWorkerOnly, "worker", false, "run only the background worker")
	serveCmd.Flags().BoolVar(&serveNoWorker, "no-worker", false, "skip the background worker (HTTP only)")
}

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := rf.log
	log.Info("anamnesia booting", "cfg", cfg.String(), "version", version)

	ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}

	emb, err := embed.New(cfg.EmbedProvider, cfg.EmbedModel, cfg.OpenAIBaseURL, cfg.OpenAIAPIKey, cfg.EmbedDims)
	if err != nil {
		return err
	}
	llmKey := cfg.AnthropicAPIKey
	if cfg.LLMProvider == "openai" {
		llmKey = cfg.OpenAIAPIKey
	}
	llmc, err := llm.New(llm.Config{
		Provider: cfg.LLMProvider,
		Model:    cfg.LLMModel,
		APIKey:   llmKey,
		BaseURL:  cfg.OpenAIBaseURL,
	})
	if err != nil {
		return err
	}
	reranker, err := retrieval.NewReranker(cfg.RerankProvider, cfg.CohereAPIKey, cfg.RerankModel)
	if err != nil {
		return err
	}
	piiDet, err := pii.New(cfg.PIIProvider, cfg.PresidioURL, pii.Mode(cfg.PIIMode))
	if err != nil {
		return err
	}

	retr := &retrieval.Engine{Store: st, Embedder: emb, Reranker: reranker}

	// MCP handler
	mcpHandler := mcp.NewHandler(mcp.Deps{
		Store:          st,
		Retrieval:      retr,
		PII:            piiDet,
		DefaultUser:    cfg.DefaultUser,
		DefaultProject: cfg.DefaultProject,
	})

	// HTTP server
	srv := httpapi.NewServer(cfg.HTTPAddr, httpapi.Deps{
		Store:          st,
		Retrieval:      retr,
		MCPHandler:     mcpHandler,
		PII:            piiDet,
		DefaultUser:    cfg.DefaultUser,
		DefaultProject: cfg.DefaultProject,
		ServerToken:    cfg.ServerToken,
		Log:            log,
	})

	// Worker
	var workerErr chan error
	if !serveNoWorker {
		workerErr = make(chan error, 1)
		worker := &jobs.Worker{
			Cfg: jobs.Config{
				EmbedEvery:       cfg.EmbedBackfill,
				ForgetEvery:      cfg.ForgetEvery,
				DecayEvery:       cfg.DecayEvery,
				ConsolidateEvery: cfg.ConsolidateEvery,
				ExtractEvery:     cfg.ExtractEvery,
			},
			Store:     st,
			Embedder:  emb,
			LLM:       llmc,
			Retrieval: retr,
			Log:       log,
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
	}

	// Graceful shutdown.
	shCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownWait)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	// Drain worker.
	if workerErr != nil {
		select {
		case <-workerErr:
		case <-time.After(cfg.ShutdownWait):
		}
	}
	return nil
}

func runMigrate(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(cmd.Context(), cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(cmd.Context()); err != nil {
		return err
	}
	rf.log.Info("migrations applied")
	return nil
}
