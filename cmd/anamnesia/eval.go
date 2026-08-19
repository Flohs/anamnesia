// eval.go — `anamnesia eval`: ingest a fixture corpus through the real
// pipeline, run labelled queries, and report what retrieval returned.
//
// It talks HTTP like any other client rather than reaching into the
// store, so it measures the path a hook actually takes. The only direct
// database access is deleting the scope it created.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/flohs/anamnesia/internal/httpapi"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
	"github.com/spf13/cobra"
)

const (
	// evalUser is a handle no real person would choose: the teardown
	// deletes everything under it, so evalScopeExists refuses to run
	// if it's ever occupied by an actual user.
	evalUser    = "anamnesia-eval"
	evalProject = "eval-corpus"
	// drainTimeout bounds the wait for extraction and embedding. A run
	// scored against a half-warm index is worse than no run.
	drainTimeout = 10 * time.Minute
)

type evalReport struct {
	At        string         `json:"at"`
	K         int            `json:"k"`
	Queries   int            `json:"queries"`
	Aggregate aggregateScore `json:"aggregate"`
	PerQuery  []queryScore   `json:"per_query"`
}

// evalMetricKs returns the recall/precision cutoffs to report for a run
// that requested k hits per query. A cutoff above k would label a metric
// computed over a truncated hit list with a k it was never given — the
// bug this guards against is `eval --k 5` reporting a "recall@10" that is
// really recall@5. If every standard cutoff exceeds k, k itself is kept
// so the report is never empty.
func evalMetricKs(k int) []int {
	var ks []int
	for _, v := range []int{1, 5, 10} {
		if v <= k {
			ks = append(ks, v)
		}
	}
	if len(ks) == 0 {
		ks = []int{k}
	}
	return ks
}

// runEvalPass ingests the corpus, waits for the queues to drain, runs
// every query and scores the result.
func runEvalPass(ctx context.Context, hc *hostConfig, sources []evalSource, queries []evalQuery, k int) (evalReport, error) {
	c := &evalClient{base: hc.ServerURL(), token: hc.Token(), hc: &http.Client{Timeout: 60 * time.Second}}

	// corpus id -> the source_id the server assigned, which is what hits
	// carry and therefore what scoring compares against.
	byCorpusID := make(map[string]string, len(sources))
	for _, s := range sources {
		occurred := s.OccurredAt
		var resp httpapi.IngestResponse
		err := c.post(ctx, "/v1/ingest", httpapi.IngestRequest{
			User: evalUser, Project: evalProject, Kind: s.Kind,
			ExternalRef: s.ID, OccurredAt: &occurred, Content: s.Content,
			PreserveRaw: true,
		}, &resp)
		if err != nil {
			return evalReport{}, fmt.Errorf("ingest %s: %w", s.ID, err)
		}
		byCorpusID[s.ID] = resp.SourceID.String()
	}

	if err := c.waitForDrain(ctx, drainTimeout); err != nil {
		return evalReport{}, err
	}

	ks := evalMetricKs(k)
	scores := make([]queryScore, 0, len(queries))
	for _, q := range queries {
		var resp httpapi.RetrieveResp
		started := time.Now()
		err := c.post(ctx, "/v1/retrieve", httpapi.HookEvent{
			User: evalUser, Project: evalProject, Prompt: q.Text, K: k, OnlyRaw: true,
		}, &resp)
		if err != nil {
			return evalReport{}, fmt.Errorf("retrieve %s: %w", q.ID, err)
		}
		latency := time.Since(started).Milliseconds()

		ranked := make([]string, 0, len(resp.Hits))
		for _, h := range resp.Hits {
			if id := hitSourceID(h); id != "" {
				ranked = append(ranked, id)
			}
		}
		want := make([]string, 0, len(q.Relevant))
		for _, r := range q.Relevant {
			want = append(want, byCorpusID[r])
		}
		scores = append(scores, score(q.ID, ranked, want, ks, latency))
	}

	return evalReport{
		K:         k,
		Queries:   len(queries),
		Aggregate: aggregate(scores, ks),
		PerQuery:  scores,
	}, nil
}

// hitSourceID reports which source a hit came from. A hit with no source
// was not extracted from the corpus — a consolidation summary, say — and
// cannot be scored against source-granularity labels.
func hitSourceID(h anamnesia.SearchHit) string {
	switch {
	case h.Fact != nil && h.Fact.SourceID != nil:
		return h.Fact.SourceID.String()
	case h.Experience != nil && h.Experience.SourceID != nil:
		return h.Experience.SourceID.String()
	}
	return ""
}

type evalClient struct {
	base  string
	token string
	hc    *http.Client
}

func (c *evalClient) post(ctx context.Context, path string, body, into any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, bytes.TrimSpace(msg))
	}
	if into == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// waitForDrain blocks until nothing is pending. Extraction is async, so
// querying before it finishes measures an index that is still filling.
func (c *evalClient) waitForDrain(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			c.base+"/v1/queue/pending?user="+evalUser+"&project="+evalProject, nil)
		if err != nil {
			return err
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 300 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
			resp.Body.Close()
			return fmt.Errorf("/v1/queue/pending: %s: %s", resp.Status, bytes.TrimSpace(msg))
		}
		var q httpapi.QueuePendingResponse
		err = json.NewDecoder(resp.Body).Decode(&q)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if q.ExtractPending == 0 && q.EmbedPending == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("queues did not drain within %s (%d to extract, %d to embed).\n"+
				"Check `anamnesia logs`: an unconfigured or failing model leaves sources pending forever",
				timeout, q.ExtractPending, q.EmbedPending)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// evalScopeExists reports whether evalUser already denotes a real user
// handle. The teardown deletes by that fixed handle, so if a real user
// ever held it, running the eval would delete their entire memory.
// Refusing to run is recoverable; deleting a stranger's memory is not.
func evalScopeExists(ctx context.Context, hc *hostConfig) (bool, error) {
	st, err := store.Open(ctx, hc.DatabaseURL())
	if err != nil {
		return false, err
	}
	defer st.Close()
	_, found, err := st.LookupUser(ctx, evalUser)
	return found, err
}

// deleteEvalScope removes everything the run created. commitments.user_id
// has no ON DELETE CASCADE (unlike every other memory table), so deleting
// the user first would abort atomically on any commitments row the eval
// corpus produced; delete those explicitly first.
func deleteEvalScope(ctx context.Context, hc *hostConfig) error {
	st, err := store.Open(ctx, hc.DatabaseURL())
	if err != nil {
		return err
	}
	defer st.Close()

	id, found, err := st.LookupUser(ctx, evalUser)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM commitments WHERE user_id = $1`, id); err != nil {
		return fmt.Errorf("delete eval commitments: %w", err)
	}

	_, err = st.DeleteUser(ctx, evalUser)
	return err
}

var (
	evalJSON     bool
	evalKeep     bool
	evalBaseline string
	evalK        int
)

var evalCmd = &cobra.Command{
	Use:   "eval",
	Short: "Measure retrieval against the built-in fixture corpus",
	Long: "Ingest a committed fixture corpus through the real pipeline, run\n" +
		"labelled queries against it, and report recall, precision, MRR and\n" +
		"latency.\n\n" +
		"The corpus is ingested under a dedicated user and deleted afterwards,\n" +
		"so a run cannot touch your own memory. It needs a configured model:\n" +
		"the stub extracts nothing and every query would score zero.",
	Args: cobra.NoArgs,
	RunE: runEval,
}

func init() {
	evalCmd.Flags().BoolVar(&evalJSON, "json", false, "emit the report as JSON")
	evalCmd.Flags().BoolVar(&evalKeep, "keep", false, "leave the ingested corpus in place")
	evalCmd.Flags().StringVar(&evalBaseline, "baseline", "", "compare against a previous --json report and fail on regression")
	evalCmd.Flags().IntVar(&evalK, "k", 10, "how many hits to request per query")
}

func runEval(cmd *cobra.Command, _ []string) error {
	if evalK <= 0 {
		return errors.New("--k must be a positive number")
	}
	hc, err := loadHostConfig()
	if err != nil {
		return err
	}
	sources, queries, err := loadCorpus()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	// The corpus is ingested, not simulated, so a run against the wrong
	// scope would write dozens of fixture rows into real memory.
	if hc.User() == evalUser {
		return fmt.Errorf("identity.user is %q, which is the scope eval deletes afterwards", evalUser)
	}

	// Teardown deletes evalUser's entire scope by that fixed handle. If a
	// real person ever holds it, an automatic delete would destroy their
	// whole memory, so a prior run left behind (crashed, killed, whatever)
	// stops this one cold rather than being cleaned up automatically.
	// Refusing to run is recoverable; deleting a stranger's memory is not.
	exists, err := evalScopeExists(ctx, hc)
	if err != nil {
		return fmt.Errorf("check for an existing eval scope: %w", err)
	}
	if exists {
		return fmt.Errorf("the %q scope already exists: a previous eval run may have crashed before cleanup.\n"+
			"Remove that user yourself before re-running eval", evalUser)
	}

	// Progress goes to stderr, not out: --json must produce nothing but the
	// report on stdout, or `eval --json > file` captures this line too and
	// leaves the file unparseable.
	fmt.Fprintf(cmd.ErrOrStderr(), "Ingesting %d sources as %s/%s\n", len(sources), evalUser, evalProject)
	report, runErr := runEvalPass(ctx, hc, sources, queries, evalK)
	if !evalKeep {
		if err := deleteEvalScope(context.WithoutCancel(ctx), hc); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not delete the eval scope: %v\n", err)
		}
	}
	if runErr != nil {
		return runErr
	}
	report.At = time.Now().UTC().Format(time.RFC3339)

	return writeEvalResult(out, cmd.ErrOrStderr(), evalJSON, report, evalBaseline)
}

// writeEvalResult writes the report to out — JSON or the rendered text,
// matching jsonMode — and, if baselinePath is set, compares against it and
// writes the explanation to errOut, never to out. A --json run is meant to
// be piped to a file (`eval --json --baseline old.json > new.json` is the
// CI invocation this is for), and anything but the report on out breaks
// that redirect the same way the progress line above once did.
func writeEvalResult(out, errOut io.Writer, jsonMode bool, report evalReport, baselinePath string) error {
	if jsonMode {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		renderReport(out, report)
	}

	if baselinePath == "" {
		return nil
	}
	raw, err := os.ReadFile(baselinePath)
	if err != nil {
		return fmt.Errorf("read baseline: %w", err)
	}
	var base evalReport
	if err := json.Unmarshal(raw, &base); err != nil {
		return fmt.Errorf("parse baseline %s: %w", baselinePath, err)
	}
	regressed, lines, err := compareToBaseline(base, report)
	if err != nil {
		return err
	}
	fmt.Fprintln(errOut)
	for _, l := range lines {
		fmt.Fprintln(errOut, l)
	}
	if regressed {
		return errors.New("retrieval regressed against the baseline")
	}
	return nil
}

// renderReport prints the aggregate, then names every query that found
// nothing. An average hides exactly the failures worth seeing.
func renderReport(w io.Writer, r evalReport) {
	a := r.Aggregate
	fmt.Fprintf(w, "\n%d queries over the fixture corpus\n\n", a.Queries)
	// Only the cutoffs the run actually computed: a run made with --k 5
	// never fetched enough hits to say anything about recall@10.
	ks := make([]int, 0, len(a.RecallAt))
	for k := range a.RecallAt {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	for _, k := range ks {
		fmt.Fprintf(w, "  recall@%-3d %.3f      precision@%-3d %.3f\n",
			k, a.RecallAt[k], k, a.PrecisionAt[k])
	}
	fmt.Fprintf(w, "\n  MRR        %.3f\n", a.MRR)
	fmt.Fprintf(w, "  latency    p50 %d ms, p95 %d ms\n", a.P50MS, a.P95MS)
	fmt.Fprintf(w, "  found nothing: %d of %d\n", a.ZeroHit, a.Queries)
	if a.ZeroHit > 0 {
		fmt.Fprintln(w, "\n  queries that returned nothing relevant:")
		for _, q := range r.PerQuery {
			if !q.Found {
				fmt.Fprintf(w, "    %s\n", q.ID)
			}
		}
	}
}

// regressionTolerance is how far a metric may drift before it counts as a
// regression. Retrieval is not deterministic — the model, the reranker and
// the embedding service all vary run to run — so a strict comparison would
// cry wolf on every invocation.
const regressionTolerance = 0.02

// compareToBaseline reports whether the run got worse, and explains every
// metric either way. It refuses to compare runs of different shape: a
// baseline taken with a different --k, or over a different query set,
// measured different things, and comparing them metric-by-metric would
// silently report on numbers that were never asking the same question.
// This also catches a baseline written before K was recorded — it reads
// as K 0, which now differs from any real run instead of comparing as if
// every one of its metrics were a genuine 0.0.
func compareToBaseline(base, now evalReport) (bool, []string, error) {
	if base.K != now.K {
		return false, nil, fmt.Errorf("baseline was run with --k %d, this run used --k %d: cannot compare", base.K, now.K)
	}
	if base.Queries != now.Queries {
		return false, nil, fmt.Errorf("baseline covered %d queries, this run covered %d: cannot compare", base.Queries, now.Queries)
	}

	var lines []string
	regressed := false
	type metric struct {
		name       string
		base, curr float64
	}
	var metrics []metric
	ks := make([]int, 0, len(now.Aggregate.RecallAt))
	for k := range now.Aggregate.RecallAt {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	for _, k := range ks {
		metrics = append(metrics,
			metric{fmt.Sprintf("recall@%d", k), base.Aggregate.RecallAt[k], now.Aggregate.RecallAt[k]},
			metric{fmt.Sprintf("precision@%d", k), base.Aggregate.PrecisionAt[k], now.Aggregate.PrecisionAt[k]},
		)
	}
	metrics = append(metrics, metric{"MRR", base.Aggregate.MRR, now.Aggregate.MRR})
	for _, m := range metrics {
		delta := m.curr - m.base
		mark := "  "
		if delta < -regressionTolerance {
			mark = "!!"
			regressed = true
		}
		lines = append(lines, fmt.Sprintf("%s %-14s %.3f -> %.3f  (%+.3f)", mark, m.name, m.base, m.curr, delta))
	}
	return regressed, lines, nil
}
