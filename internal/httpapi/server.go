// Package httpapi hosts the local HTTP surface exposed to the host-side
// CLI hooks and to Claude Code's MCP client.
//
// Routes:
//
//	GET  /v1/health          health probe (no auth)
//	POST /v1/sessions/start  SessionStart hook
//	POST /v1/retrieve        UserPromptSubmit hook (hybrid search)
//	POST /v1/ingest          Stop / PreCompact hook + generic write (async extraction)
//	POST /v1/experience      direct experience write (RAG mode — bypass extractor)
//	GET  /v1/queue/pending   pending counts for one user (extract + embed)
//	POST /mcp                MCP transport (streamable-http)
//
// Auth is optional: if Config.ServerToken is set, every request must
// carry it as `Authorization: Bearer <token>`. Otherwise the API is
// unauthenticated — appropriate for a localhost-only docker stack.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia-open-source/internal/jobs"
	"github.com/flohs/anamnesia-open-source/internal/pii"
	"github.com/flohs/anamnesia-open-source/internal/retrieval"
	"github.com/flohs/anamnesia-open-source/internal/store"
	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

// Deps bundles the runtime collaborators handed to the HTTP layer.
type Deps struct {
	Store          *store.Store
	Retrieval      *retrieval.Engine
	PII            pii.Detector
	Briefer        *jobs.Briefer // nil-safe; nil falls back to a stub summary
	MCPHandler     http.Handler  // mounted at /mcp
	DefaultUser    string
	DefaultProject string
	ServerToken    string // empty = no auth required
	Log            *slog.Logger
}

// NewServer returns a configured *http.Server bound to addr.
func NewServer(addr string, d Deps) *http.Server {
	mux := http.NewServeMux()

	mux.Handle("/v1/health", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "anamnesia"})
	}))

	mux.Handle("/v1/sessions/start", d.protect(http.HandlerFunc(d.handleSessionStart)))
	mux.Handle("/v1/retrieve", d.protect(http.HandlerFunc(d.handleRetrieve)))
	mux.Handle("/v1/ingest", d.protect(http.HandlerFunc(d.handleIngest)))
	mux.Handle("/v1/queue/pending", d.protect(http.HandlerFunc(d.handleQueuePending)))
	mux.Handle("/v1/experience", d.protect(http.HandlerFunc(d.handleExperience)))
	mux.Handle("/v1/identity", d.protect(http.HandlerFunc(d.handleIdentity)))
	mux.Handle("/v1/briefing", d.protect(http.HandlerFunc(d.handleBriefing)))
	mux.Handle("/v1/people", d.protect(http.HandlerFunc(d.handlePeople)))
	mux.Handle("/v1/capabilities", d.protect(http.HandlerFunc(d.handleCapabilities)))
	mux.Handle("/v1/commitments", d.protect(http.HandlerFunc(d.handleCommitments)))
	mux.Handle("/v1/commitments/resolve", d.protect(http.HandlerFunc(d.handleCommitmentResolve)))
	mux.Handle("/v1/audit", d.protect(http.HandlerFunc(d.handleAudit)))

	if d.MCPHandler != nil {
		mux.Handle("/mcp", d.protect(d.MCPHandler))
		mux.Handle("/mcp/", d.protect(d.MCPHandler))
	}

	return &http.Server{
		Addr:              addr,
		Handler:           withLogging(d.Log, mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

func (d Deps) protect(next http.Handler) http.Handler {
	if d.ServerToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		want := "Bearer " + d.ServerToken
		if auth != want {
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	if log == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.status,
			"ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }

// ─── handlers ────────────────────────────────────────────────────────

// HookEvent is the union payload the CLI sends for any hook. Empty
// fields are common; each handler picks out what it needs.
type HookEvent struct {
	SessionID      string `json:"session_id,omitempty"`
	CWD            string `json:"cwd,omitempty"`
	Project        string `json:"project,omitempty"`
	User           string `json:"user,omitempty"`
	Prompt         string `json:"prompt,omitempty"`
	MaxFacts       int    `json:"max_facts,omitempty"`
	MaxExperiences int    `json:"max_experiences,omitempty"`
	K              int    `json:"k,omitempty"`
	// OnlyRaw on /v1/retrieve restricts experience hits to abstraction=0
	// (verbatim sources). Set by benchmarks / citation flows; leave
	// false for context injection where summaries are useful.
	OnlyRaw bool `json:"only_raw,omitempty"`
}

func (d Deps) resolveScope(ctx context.Context, ev *HookEvent) (anamnesia.Scope, error) {
	user := ev.User
	if user == "" {
		user = d.DefaultUser
	}
	if user == "" {
		user = "default"
	}
	uid, err := d.Store.EnsureUser(ctx, user)
	if err != nil {
		return anamnesia.Scope{}, fmt.Errorf("ensure user: %w", err)
	}
	scope := anamnesia.Scope{UserID: uid}
	proj := ev.Project
	if proj == "" {
		proj = d.DefaultProject
	}
	if proj != "" {
		pid, err := d.Store.EnsureProject(ctx, uid, proj)
		if err != nil {
			return anamnesia.Scope{}, fmt.Errorf("ensure project: %w", err)
		}
		scope.ProjectID = &pid
	}
	return scope, nil
}

// SessionStartResp is the markdown / structured payload Claude Code
// receives at SessionStart.
type SessionStartResp struct {
	Facts        []*anamnesia.Fact       `json:"facts"`
	Experiences  []*anamnesia.Experience `json:"experiences"`
	PersonaBlock string                  `json:"persona_block,omitempty"`
	Hint         string                  `json:"hint,omitempty"`
}

func (d Deps) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	var ev HookEvent
	if err := readJSON(r, &ev); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ev.MaxFacts <= 0 {
		ev.MaxFacts = 50
	}
	if ev.MaxExperiences <= 0 {
		ev.MaxExperiences = 10
	}
	facts, err := d.Store.ListFacts(r.Context(), scope, "", ev.MaxFacts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	exps, err := d.Store.ListExperiences(r.Context(), scope, ev.MaxExperiences)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, err := d.Store.GetIdentity(r.Context(), scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, SessionStartResp{
		Facts: facts, Experiences: exps, PersonaBlock: id.SystemPrompt,
	})
}

// ─── identity ────────────────────────────────────────────────────────

func (d Deps) handleIdentity(w http.ResponseWriter, r *http.Request) {
	ev := HookEvent{
		User:    r.URL.Query().Get("user"),
		Project: r.URL.Query().Get("project"),
	}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, err := d.Store.GetIdentity(r.Context(), scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, id)
}

// RetrieveResp is returned for the UserPromptSubmit hook. Hits are the
// primary, current-project results. CrossProject is a small, separately
// rendered set of hits from the user's *other* projects that match the
// prompt — the "wait, you touched this elsewhere" surface. It is only
// populated when a current project is set.
type RetrieveResp struct {
	Hits         []anamnesia.SearchHit `json:"hits"`
	CrossProject []CrossProjectHit     `json:"cross_project,omitempty"`
}

// CrossProjectHit is a prompt-matching memory from a project other than
// the one the user is currently in, labelled with that project's slug
// and how recently it happened.
type CrossProjectHit struct {
	Domain  string `json:"domain"`
	Title   string `json:"title"`
	Project string `json:"project"`
	Recency string `json:"recency,omitempty"`
}

// crossProjectMax caps how many other-project hits we surface per prompt.
// Kept tight on purpose: this fires on every submit, so noise control
// matters more than recall here.
const crossProjectMax = 2

// crossProjectMinScore is the relevance floor for surfacing an
// other-project hit. It only applies when a reranker ran (RerankerRank>0),
// because then SearchHit.Score is the reranker's absolute relevance
// (0..1, see retrieval/rerank.go). Without a reranker, Score is a tiny
// RRF value that can't be thresholded, so we fall back to the cap alone.
const crossProjectMinScore = 0.5

func (d Deps) handleRetrieve(w http.ResponseWriter, r *http.Request) {
	var ev HookEvent
	if err := readJSON(r, &ev); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(ev.Prompt) == "" {
		writeJSON(w, http.StatusOK, RetrieveResp{Hits: nil})
		return
	}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	k := ev.K
	if k <= 0 {
		k = 10
	}
	hits, err := d.Retrieval.Search(r.Context(), retrieval.Query{
		Scope: scope, Text: ev.Prompt, K: k, OnlyRaw: ev.OnlyRaw,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := RetrieveResp{Hits: hits}
	// Only contrast against other projects when there's a current project
	// to contrast with.
	if scope.ProjectID != nil {
		resp.CrossProject = d.crossProjectHits(r.Context(), scope, ev.Prompt, ev.OnlyRaw)
	}
	writeJSON(w, http.StatusOK, resp)
}

// crossProjectHits runs a second, user-wide search (no project filter)
// and keeps the top matches that belong to a project *other* than the
// current one. User-level (project-null) hits are excluded — those
// already surface in the primary results.
func (d Deps) crossProjectHits(ctx context.Context, scope anamnesia.Scope, prompt string, onlyRaw bool) []CrossProjectHit {
	userScope := anamnesia.Scope{UserID: scope.UserID} // ProjectID nil → all projects
	hits, err := d.Retrieval.Search(ctx, retrieval.Query{
		Scope: userScope, Text: prompt, K: 8, OnlyRaw: onlyRaw,
	})
	if err != nil {
		return nil
	}
	now := time.Now().UTC()
	slugCache := map[uuid.UUID]string{}
	var out []CrossProjectHit
	for _, h := range hits {
		// Relevance floor — only when the reranker assigned an absolute
		// score. This is what keeps unrelated other-project memories from
		// leaking onto every prompt.
		if h.RerankerRank > 0 && h.Score < crossProjectMinScore {
			continue
		}
		pid, title, when, ok := crossFields(h)
		if !ok || pid == nil {
			continue // unsupported domain or user-level fact
		}
		if scope.ProjectID != nil && *pid == *scope.ProjectID {
			continue // same project as primary
		}
		slug, cached := slugCache[*pid]
		if !cached {
			s, err := d.Store.LookupProjectSlug(ctx, *pid)
			if err != nil {
				continue
			}
			slug = s
			slugCache[*pid] = s
		}
		out = append(out, CrossProjectHit{
			Domain:  string(h.Domain),
			Title:   title,
			Project: slug,
			Recency: humanRecency(when, now),
		})
		if len(out) >= crossProjectMax {
			break
		}
	}
	return out
}

// crossFields pulls the (projectID, title, when) needed to label a
// cross-project hit. Returns ok=false for domains we don't surface here
// (skills carry no useful recency for this view).
func crossFields(h anamnesia.SearchHit) (pid *uuid.UUID, title string, when time.Time, ok bool) {
	switch h.Domain {
	case anamnesia.DomainFact:
		if h.Fact != nil {
			return h.Fact.Scope.ProjectID, h.Fact.Key, h.Fact.IngestedAt, true
		}
	case anamnesia.DomainExperience:
		if h.Experience != nil {
			w := h.Experience.IngestedAt
			if h.Experience.OccurredAt != nil {
				w = *h.Experience.OccurredAt
			}
			t := h.Experience.Title
			if t == "" {
				t = firstLineN(h.Experience.Body, 80)
			}
			return h.Experience.Scope.ProjectID, t, w, true
		}
	}
	return nil, "", time.Time{}, false
}

// humanRecency renders a coarse, human-friendly "when" for a hit.
func humanRecency(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	days := int(now.Sub(t).Hours() / 24)
	switch {
	case days <= 0:
		return "today"
	case days == 1:
		return "yesterday"
	case days < 7:
		return fmt.Sprintf("%d days ago", days)
	case days < 14:
		return "last week"
	default:
		return t.Format("2006-01-02")
	}
}

func firstLineN(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// IngestRequest is the body shape of POST /v1/ingest. Source-kind is a
// free-form hint; the extractor doesn't branch on it. Content is the
// raw text we should consider for extraction; participants/title/etc.
// are optional metadata.
type IngestRequest struct {
	User         string         `json:"user,omitempty"`
	Project      string         `json:"project,omitempty"`
	Kind         string         `json:"kind"`
	ExternalRef  string         `json:"external_ref,omitempty"`
	Title        string         `json:"title,omitempty"`
	Participants []string       `json:"participants,omitempty"`
	OccurredAt   *time.Time     `json:"occurred_at,omitempty"`
	Content      string         `json:"content"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	PreserveRaw  bool           `json:"preserve_raw,omitempty"`
}

// IngestResponse confirms the source was queued for extraction.
type IngestResponse struct {
	SourceID  uuid.UUID `json:"source_id"`
	Queued    bool      `json:"queued"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (d Deps) handleIngest(w http.ResponseWriter, r *http.Request) {
	var req IngestRequest
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Kind == "" {
		http.Error(w, "kind required", http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, "content required", http.StatusBadRequest)
		return
	}
	ev := HookEvent{User: req.User, Project: req.Project}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// PII scrub at ingest. If the user wants the raw content preserved
	// (preserve_raw=true), we still scrub — the tags survive but the
	// raw text doesn't.
	content := req.Content
	if d.PII != nil {
		cleaned, _, err := d.PII.Scrub(r.Context(), content)
		if err == nil {
			content = cleaned
		}
	}
	occurred := time.Now().UTC()
	if req.OccurredAt != nil {
		occurred = *req.OccurredAt
	}
	src := &anamnesia.Source{
		Scope:        scope,
		Kind:         req.Kind,
		ExternalRef:  req.ExternalRef,
		Title:        req.Title,
		Participants: req.Participants,
		OccurredAt:   occurred,
		RawContent:   content,
		Metadata:     req.Metadata,
		PreserveRaw:  req.PreserveRaw,
	}
	if err := d.Store.InsertSource(r.Context(), src); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusAccepted, IngestResponse{
		SourceID: src.ID, Queued: true, ExpiresAt: src.ExpiresAt,
	})
}

// QueuePendingResponse reports how much background work is still in
// flight for one user. `extract_pending` is the number of sources still
// waiting for the extractor; `embed_pending` is the number of facts +
// experiences + entities still missing an embedding. Callers
// (benchmarks, batch jobs) poll this between an /ingest burst and a
// /retrieve to know retrieval is fully warm.
type QueuePendingResponse struct {
	ExtractPending int `json:"extract_pending"`
	EmbedPending   int `json:"embed_pending"`
}

func (d Deps) handleQueuePending(w http.ResponseWriter, r *http.Request) {
	ev := HookEvent{
		User:    r.URL.Query().Get("user"),
		Project: r.URL.Query().Get("project"),
	}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	extract, embed, err := d.Store.QueuePending(r.Context(), scope.UserID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, QueuePendingResponse{
		ExtractPending: extract,
		EmbedPending:   embed,
	})
}

// ExperienceRequest writes one experience row directly, bypassing the
// extractor. Used by RAG-style benchmarks that want the source content
// itself in memory rather than LLM-distilled facts. Body is embedded
// inline (one embed call) before insert so retrieval is warm the
// instant this returns — there is no extract worker, no embed
// backfill wait.
type ExperienceRequest struct {
	User         string         `json:"user,omitempty"`
	Project      string         `json:"project,omitempty"`
	Title        string         `json:"title,omitempty"`
	Body         string         `json:"body"`
	Kind         string         `json:"kind,omitempty"`        // case | strategy | hybrid (default: case)
	Topic        string         `json:"topic,omitempty"`
	Participants []string       `json:"participants,omitempty"`
	OccurredAt   *time.Time     `json:"occurred_at,omitempty"`
	Importance   float32        `json:"importance,omitempty"`
	Meta         map[string]any `json:"meta,omitempty"`
}

type ExperienceResponse struct {
	ExperienceID uuid.UUID `json:"experience_id"`
}

func (d Deps) handleExperience(w http.ResponseWriter, r *http.Request) {
	var req ExperienceRequest
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		http.Error(w, "body required", http.StatusBadRequest)
		return
	}
	scope, err := d.resolveScope(r.Context(), &HookEvent{User: req.User, Project: req.Project})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body := req.Body
	var piiTags []string
	if d.PII != nil {
		cleaned, tags, err := d.PII.Scrub(r.Context(), body)
		if err == nil {
			body = cleaned
			piiTags = tags
		}
	}
	exp := &anamnesia.Experience{
		Scope:        scope,
		Kind:         anamnesia.ExperienceKind(req.Kind),
		Title:        req.Title,
		Body:         body,
		Topic:        req.Topic,
		Participants: req.Participants,
		OccurredAt:   req.OccurredAt,
		Importance:   req.Importance,
		Meta:         req.Meta,
		PIITags:      piiTags,
	}
	if d.Retrieval != nil && d.Retrieval.Embedder != nil {
		vecs, err := d.Retrieval.Embedder.Embed(r.Context(), []string{body})
		if err == nil && len(vecs) == 1 && len(vecs[0]) > 0 {
			exp.Embedding = vecs[0]
			exp.EmbedModel = d.Retrieval.Embedder.Model()
		} else if err != nil && d.Log != nil {
			d.Log.Warn("experience: inline embed failed (will be backfilled)", "err", err)
		}
	}
	if err := d.Store.RecordExperience(r.Context(), exp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, ExperienceResponse{ExperienceID: exp.ID})
}

// ─── audit ───────────────────────────────────────────────────────────

func (d Deps) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	if subject := r.URL.Query().Get("subject"); subject != "" {
		parts := strings.SplitN(subject, ":", 2)
		if len(parts) != 2 {
			http.Error(w, `subject must be "<kind>:<uuid>"`, http.StatusBadRequest)
			return
		}
		id, err := uuid.Parse(parts[1])
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out, err := d.Store.AuditForSubject(r.Context(), parts[0], id, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	ev := HookEvent{User: r.URL.Query().Get("user"), Project: r.URL.Query().Get("project")}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out, err := d.Store.AuditTail(r.Context(), scope, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── commitments ─────────────────────────────────────────────────────

type commitmentReq struct {
	User        string     `json:"user,omitempty"`
	Project     string     `json:"project,omitempty"`
	Owner       string     `json:"owner,omitempty"`
	Beneficiary string     `json:"beneficiary,omitempty"`
	Body        string     `json:"body"`
	DueAt       *time.Time `json:"due_at,omitempty"`
}

func (d Deps) handleCommitments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ev := HookEvent{User: r.URL.Query().Get("user"), Project: r.URL.Query().Get("project")}
		scope, err := d.resolveScope(r.Context(), &ev)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		status := anamnesia.CommitmentStatus(r.URL.Query().Get("status"))
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			fmt.Sscanf(v, "%d", &limit)
		}
		out, err := d.Store.ListCommitments(r.Context(), scope, status, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var req commitmentReq
		if err := readJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Body == "" {
			http.Error(w, "body required", http.StatusBadRequest)
			return
		}
		scope, err := d.resolveScope(r.Context(), &HookEvent{User: req.User, Project: req.Project})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c := &anamnesia.Commitment{
			Scope: scope, Owner: req.Owner, Beneficiary: req.Beneficiary,
			Body: req.Body, DueAt: req.DueAt, Status: anamnesia.CommitmentOpen,
		}
		if err := d.Store.RecordCommitment(r.Context(), c); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, c)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type resolveReq struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (d Deps) handleCommitmentResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req resolveReq
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(req.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := d.Store.ResolveCommitment(r.Context(), id, anamnesia.CommitmentStatus(req.Status)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── capabilities ────────────────────────────────────────────────────

func (d Deps) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	ev := HookEvent{User: r.URL.Query().Get("user"), Project: r.URL.Query().Get("project")}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	out, err := d.Store.ListCapabilities(r.Context(), scope, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── people ──────────────────────────────────────────────────────────

func (d Deps) handlePeople(w http.ResponseWriter, r *http.Request) {
	ev := HookEvent{
		User: r.URL.Query().Get("user"), Project: r.URL.Query().Get("project"),
	}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	people, err := d.Store.ListPeople(r.Context(), scope, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, people)
}

// ─── briefing ────────────────────────────────────────────────────────

type briefingReq struct {
	User        string    `json:"user,omitempty"`
	Project     string    `json:"project,omitempty"`
	Since       time.Time `json:"since"`
	Until       time.Time `json:"until,omitempty"`
	Topic       string    `json:"topic,omitempty"`
	MaxAdjacent int       `json:"max_adjacent,omitempty"`
}

func (d Deps) handleBriefing(w http.ResponseWriter, r *http.Request) {
	var req briefingReq
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Since.IsZero() {
		http.Error(w, "since required", http.StatusBadRequest)
		return
	}
	ev := HookEvent{User: req.User, Project: req.Project}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	inWin, err := d.Store.ListExperiencesInWindow(r.Context(), scope, req.Since, req.Until, req.Topic, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	flankSince := req.Since.AddDate(0, 0, -14)
	flankUntil := req.Until
	if flankUntil.IsZero() {
		flankUntil = time.Now().UTC()
	}
	flankUntil = flankUntil.AddDate(0, 0, 14)
	adj, err := d.Store.ListExperiencesInWindow(r.Context(), scope, flankSince, flankUntil, req.Topic, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	inSet := map[uuid.UUID]bool{}
	for _, e := range inWin {
		inSet[e.ID] = true
	}
	adjOut := adj[:0]
	for _, e := range adj {
		if !inSet[e.ID] {
			adjOut = append(adjOut, e)
		}
	}
	var briefer jobs.Briefer
	if d.Briefer != nil {
		briefer = *d.Briefer
	}
	resp, err := briefer.Brief(r.Context(), anamnesia.BriefingRequest{
		Scope: scope, Since: req.Since, Until: req.Until,
		Topic: req.Topic, MaxAdjacent: req.MaxAdjacent,
	}, inWin, adjOut)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── helpers ─────────────────────────────────────────────────────────

func readJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("parse body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

