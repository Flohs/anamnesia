// Package jobs is the background worker. It runs three lightweight loops:
//   - embed:    backfill missing vectors on facts/experiences/entities
//   - forget:   purge expired working memory
//   - consolidate: distil clusters of experiences with the LLM
//
// All three are restartable; nothing here keeps in-memory state.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/decay"
	"github.com/flohs/anamnesia/internal/embed"
	"github.com/flohs/anamnesia/internal/extract"
	"github.com/flohs/anamnesia/internal/llm"
	"github.com/flohs/anamnesia/internal/retrieval"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// Config governs the worker loop cadence.
type Config struct {
	EmbedEvery       time.Duration
	ForgetEvery      time.Duration
	DecayEvery       time.Duration
	ConsolidateEvery time.Duration
	ExtractEvery     time.Duration
	ExtractBatch     int
	// ExtractConcurrency is how many sources in a batch are extracted at
	// once. Extraction is mostly network wait, so this is close to a
	// linear speedup. It is not free of meaning: sources in one batch
	// stop seeing each other's facts as merge candidates, so a bulk
	// backfill of related sessions can produce duplicates a serial drain
	// would have merged. Default 1, which is what it did before.
	ExtractConcurrency int
	EmbedBatch         int
	Consolidate        ConsolidateConfig
	Decay              decay.Config
	Extract            extract.Config
}

// Worker holds the dependencies for the background loops.
type Worker struct {
	Cfg       Config
	Store     *store.Store
	Embedder  embed.Embedder
	LLM       llm.Client
	Retrieval *retrieval.Engine
	Log       *slog.Logger
	// Activity records how each tick went. Nil is a working no-op, so
	// the worker never depends on anyone watching.
	Activity *activity.Recorder
}

// Run blocks until ctx is cancelled. Each loop sleeps independently.
func (w *Worker) Run(ctx context.Context) error {
	if w.Cfg.EmbedBatch <= 0 {
		w.Cfg.EmbedBatch = 32
	}
	if w.Cfg.EmbedEvery <= 0 {
		w.Cfg.EmbedEvery = time.Minute
	}
	if w.Cfg.ForgetEvery <= 0 {
		w.Cfg.ForgetEvery = time.Hour
	}
	if w.Cfg.DecayEvery <= 0 {
		w.Cfg.DecayEvery = time.Hour
	}
	if w.Cfg.ConsolidateEvery <= 0 {
		w.Cfg.ConsolidateEvery = 24 * time.Hour
	}
	if w.Cfg.ExtractEvery <= 0 {
		w.Cfg.ExtractEvery = 15 * time.Second
	}
	if w.Cfg.ExtractBatch <= 0 {
		w.Cfg.ExtractBatch = 16
	}

	go w.loop(ctx, "embed", w.Cfg.EmbedEvery, w.tickEmbed)
	go w.loop(ctx, "forget", w.Cfg.ForgetEvery, w.tickForget)
	go w.loop(ctx, "decay", w.Cfg.DecayEvery, w.tickDecay)
	go w.loop(ctx, "consolidate", w.Cfg.ConsolidateEvery, w.tickConsolidate)
	go w.loop(ctx, "extract", w.Cfg.ExtractEvery, w.tickExtract)
	go w.loop(ctx, "purge-sources", time.Hour, w.tickPurgeSources)

	<-ctx.Done()
	return ctx.Err()
}

func (w *Worker) loop(ctx context.Context, name string, every time.Duration, fn func(context.Context) (string, error)) {
	w.Activity.SetInterval(name, every)
	t := time.NewTicker(every)
	defer t.Stop()
	// Fire once immediately on start.
	w.tick(ctx, name, fn)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx, name, fn)
		}
	}
}

// tick runs one pass and records how it went. Every tick returns a short
// sentence saying what it did, which is the difference between a worker
// lane that reads and one that only proves the process is alive.
func (w *Worker) tick(ctx context.Context, name string, fn func(context.Context) (string, error)) {
	done := w.Activity.LoopStart(name)
	result, err := fn(ctx)
	done(result, err)
	if err != nil {
		w.Log.Warn("worker tick", "loop", name, "err", err)
	}
}

func (w *Worker) tickEmbed(ctx context.Context) (string, error) {
	if w.Embedder == nil {
		return "no embedder configured", nil
	}
	// Backfill facts.
	facts, err := w.Store.FactsMissingEmbedding(ctx, w.Cfg.EmbedBatch)
	if err != nil {
		return "", fmt.Errorf("fetch facts: %w", err)
	}
	if len(facts) > 0 {
		texts := make([]string, len(facts))
		for i, f := range facts {
			texts[i] = factText(f.Key, f.Value)
		}
		vecs, err := w.Embedder.Embed(ctx, texts)
		if err != nil {
			return "", fmt.Errorf("embed facts: %w", err)
		}
		for i, f := range facts {
			if i >= len(vecs) || vecs[i] == nil {
				continue
			}
			if err := w.Store.SetFactEmbedding(ctx, f.ID, vecs[i], w.Embedder.Model()); err != nil {
				w.Log.Warn("set fact embedding", "id", f.ID, "err", err)
			}
		}
		w.Log.Info("embedded facts", "n", len(facts))
	}
	// Backfill experiences.
	exps, err := w.Store.ExperiencesMissingEmbedding(ctx, w.Cfg.EmbedBatch)
	if err != nil {
		return "", fmt.Errorf("fetch experiences: %w", err)
	}
	if len(exps) > 0 {
		texts := make([]string, len(exps))
		for i, e := range exps {
			texts[i] = expText(e.Title, e.Body)
		}
		vecs, err := w.Embedder.Embed(ctx, texts)
		if err != nil {
			return "", fmt.Errorf("embed experiences: %w", err)
		}
		for i, e := range exps {
			if i >= len(vecs) || vecs[i] == nil {
				continue
			}
			if err := w.Store.SetExperienceEmbedding(ctx, e.ID, vecs[i], w.Embedder.Model()); err != nil {
				w.Log.Warn("set experience embedding", "id", e.ID, "err", err)
			}
		}
		w.Log.Info("embedded experiences", "n", len(exps))
	}
	// Backfill entities. The text is the entity NAME, matching what the
	// graph extractor embeds when it creates one, because that vector's
	// only job is merge-candidate recall — an entity without one can
	// never be offered as a candidate, so entity resolution is silently
	// inert for it forever. That state is not rare: any entity created
	// while the embedder was down has it, and `migrate --dims N` (the
	// documented repair for a width mismatch) does ALTER COLUMN ... USING
	// NULL, which puts every entity in the database into it at once.
	// QueuePending also counts these into embed_pending, so without this
	// backfill anything waiting for the queue to drain waits forever.
	ents, err := w.Store.EntitiesMissingEmbedding(ctx, w.Cfg.EmbedBatch)
	if err != nil {
		return "", fmt.Errorf("fetch entities: %w", err)
	}
	if len(ents) > 0 {
		texts := make([]string, len(ents))
		for i, e := range ents {
			texts[i] = e.Name
		}
		vecs, err := w.Embedder.Embed(ctx, texts)
		if err != nil {
			return "", fmt.Errorf("embed entities: %w", err)
		}
		for i, e := range ents {
			if i >= len(vecs) || vecs[i] == nil {
				continue
			}
			if err := w.Store.SetEntityEmbedding(ctx, e.ID, vecs[i]); err != nil {
				w.Log.Warn("set entity embedding", "id", e.ID, "err", err)
			}
		}
		w.Log.Info("embedded entities", "n", len(ents))
	}
	// Backfill artifacts. The text is the label plus the page's own
	// content, so retrieval matches what the artifact says rather than
	// only the sentence it was published with. An artifact recovered from
	// a transcript has no body and is embedded on its label alone, which
	// is thin but still findable.
	arts, err := w.Store.ArtifactsMissingEmbedding(ctx, w.Cfg.EmbedBatch)
	if err != nil {
		return "", fmt.Errorf("fetch artifacts: %w", err)
	}
	if len(arts) > 0 {
		texts := make([]string, len(arts))
		for i, a := range arts {
			texts[i] = expText(a.Label(), a.Body)
		}
		vecs, err := w.Embedder.Embed(ctx, texts)
		if err != nil {
			return "", fmt.Errorf("embed artifacts: %w", err)
		}
		for i, a := range arts {
			if i >= len(vecs) || vecs[i] == nil {
				continue
			}
			if err := w.Store.SetArtifactEmbedding(ctx, a.ID, vecs[i], w.Embedder.Model()); err != nil {
				w.Log.Warn("set artifact embedding", "id", a.ID, "err", err)
			}
		}
		w.Log.Info("embedded artifacts", "n", len(arts))
	}
	if len(facts) == 0 && len(exps) == 0 && len(ents) == 0 && len(arts) == 0 {
		return "nothing to embed", nil
	}
	w.publishQueues(ctx)
	return fmt.Sprintf("embedded %d facts, %d experiences, %d entities, %d artifacts",
		len(facts), len(exps), len(ents), len(arts)), nil
}

func (w *Worker) tickForget(ctx context.Context) (string, error) {
	n, err := w.Store.PurgeExpiredWorking(ctx)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "nothing expired", nil
	}
	w.Log.Info("purged expired working memory", "n", n)
	return fmt.Sprintf("purged %d expired working entries", n), nil
}

func (w *Worker) tickDecay(ctx context.Context) (string, error) {
	d := &decay.Worker{Store: w.Store, Log: w.Log, Cfg: w.Cfg.Decay}
	if err := d.Tick(ctx); err != nil {
		return "", err
	}
	return "recomputed experience relevance", nil
}

func (w *Worker) tickConsolidate(ctx context.Context) (string, error) {
	if w.LLM == nil {
		return "no llm configured", nil
	}
	if err := ConsolidationRun(ctx, w.Store, w.LLM, w.Cfg.Consolidate, w.Log, w.Cfg.ConsolidateEvery, w.Activity); err != nil {
		return "", err
	}
	return "consolidation pass complete", nil
}

func (w *Worker) tickExtract(ctx context.Context) (string, error) {
	if w.LLM == nil {
		return "no llm configured", nil
	}
	pending, err := w.Store.ListPendingSources(ctx, w.Cfg.ExtractBatch)
	if err != nil {
		return "", err
	}
	if len(pending) == 0 {
		return "no pending sources", nil
	}
	ex := &extract.Extractor{
		Cfg:       w.Cfg.Extract,
		Store:     w.Store,
		Embedder:  w.Embedder,
		Retrieval: w.Retrieval,
		LLM:       w.LLM,
		Log:       w.Log,
		Activity:  w.Activity,
	}
	// Per-source outcomes land in a preallocated slice by index, so the
	// aggregation below needs no lock however many workers ran.
	type outcome struct {
		ops    int
		failed bool
	}
	outcomes := make([]outcome, len(pending))
	forEachLimited(ctx, pending, w.Cfg.ExtractConcurrency, func(ctx context.Context, i int, src *anamnesia.Source) {
		ops, err := ex.Run(ctx, src)
		if err != nil {
			outcomes[i] = outcome{failed: true}
			if w.Log != nil {
				w.Log.Warn("extract failed", "source", src.ID, "err", err)
			}
			_ = w.Store.MarkFailed(ctx, src.ID, err.Error())
			return
		}
		if ops == 0 {
			_ = w.Store.MarkSkipped(ctx, src.ID)
		} else {
			_ = w.Store.MarkExtracted(ctx, src.ID, ops)
		}
		outcomes[i] = outcome{ops: ops}
		if w.Log != nil && ops > 0 {
			w.Log.Info("extracted", "source", src.ID, "kind", src.Kind, "ops", ops)
		}
	})
	operations, failed := 0, 0
	for _, o := range outcomes {
		operations += o.ops
		if o.failed {
			failed++
		}
	}
	w.publishQueues(ctx)
	result := fmt.Sprintf("%d sources, %d operations", len(pending), operations)
	if failed > 0 {
		result += fmt.Sprintf(", %d failed", failed)
	}
	return result, nil
}

// publishQueues announces the new queue depth to anyone watching the
// stream. Called from the ticks that drain a queue, never from one that
// found nothing to do: an idle install would otherwise run two COUNTs a
// second to report a number that has not moved.
func (w *Worker) publishQueues(ctx context.Context) {
	if w.Activity == nil || w.Store == nil {
		return
	}
	extract, embed, err := w.Store.QueuePendingAll(ctx)
	if err != nil {
		return // depth is a nicety; failing to read it is not worth a log line
	}
	w.Activity.PublishQueues(extract, embed)
}

func (w *Worker) tickPurgeSources(ctx context.Context) (string, error) {
	n, err := w.Store.PurgeExpiredSourceContent(ctx)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "nothing to purge", nil
	}
	if w.Log != nil {
		w.Log.Info("purged expired source content", "n", n)
	}
	return fmt.Sprintf("purged raw content from %d sources", n), nil
}

func factText(key string, value map[string]any) string {
	if len(value) == 0 {
		return key
	}
	return key
}

func expText(title, body string) string {
	if title != "" {
		return title + "\n\n" + body
	}
	return body
}
