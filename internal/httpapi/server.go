// Package httpapi hosts the local HTTP surface exposed to the host-side
// CLI hooks and to Claude Code's MCP client.
//
// Routes:
//
//	GET  /v1/health          health probe (no auth)
//	POST /v1/sessions/start  SessionStart hook
//	POST /v1/retrieve        UserPromptSubmit hook (hybrid search)
//	POST /v1/capture         PostToolUse hook (append working memory)
//	POST /v1/sessions/end    Stop hook (fold session → experience)
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
	MCPHandler     http.Handler // mounted at /mcp
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
	mux.Handle("/v1/capture", d.protect(http.HandlerFunc(d.handleCapture)))
	mux.Handle("/v1/sessions/end", d.protect(http.HandlerFunc(d.handleSessionEnd)))
	mux.Handle("/v1/ingest", d.protect(http.HandlerFunc(d.handleIngest)))

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
	SessionID      string         `json:"session_id,omitempty"`
	CWD            string         `json:"cwd,omitempty"`
	Project        string         `json:"project,omitempty"`
	User           string         `json:"user,omitempty"`
	Prompt         string         `json:"prompt,omitempty"`
	ToolName       string         `json:"tool_name,omitempty"`
	ToolInput      map[string]any `json:"tool_input,omitempty"`
	ToolResponse   map[string]any `json:"tool_response,omitempty"`
	MaxFacts       int            `json:"max_facts,omitempty"`
	MaxExperiences int            `json:"max_experiences,omitempty"`
	K              int            `json:"k,omitempty"`
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
	Facts       []*anamnesia.Fact       `json:"facts"`
	Experiences []*anamnesia.Experience `json:"experiences"`
	Hint        string                  `json:"hint,omitempty"`
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
	writeJSON(w, http.StatusOK, SessionStartResp{Facts: facts, Experiences: exps})
}

// RetrieveResp is returned for the UserPromptSubmit hook.
type RetrieveResp struct {
	Hits []anamnesia.SearchHit `json:"hits"`
}

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
		Scope: scope, Text: ev.Prompt, K: k,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, RetrieveResp{Hits: hits})
}

func (d Deps) handleCapture(w http.ResponseWriter, r *http.Request) {
	var ev HookEvent
	if err := readJSON(r, &ev); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isReadOnlyTool(ev.ToolName) {
		writeJSON(w, http.StatusOK, map[string]any{"skipped": "read-only tool"})
		return
	}
	scope, err := d.resolveScope(r.Context(), &ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sid, err := uuid.Parse(ev.SessionID)
	if err != nil {
		sid = uuid.New()
	}
	body := summariseToolUse(ev.ToolName, ev.ToolInput, ev.ToolResponse)
	// PII scrub before we persist. Tags land in meta so they're visible
	// to retrieval but don't pollute the body column.
	var piiTags []string
	if d.PII != nil {
		cleaned, tags, err := d.PII.Scrub(r.Context(), body)
		if err == nil {
			body = cleaned
			piiTags = tags
		}
	}
	meta := map[string]any{
		"tool":  ev.ToolName,
		"input": ev.ToolInput,
	}
	if len(piiTags) > 0 {
		meta["pii_tags"] = piiTags
	}
	entry := &anamnesia.WorkingEntry{
		Scope:     scope,
		SessionID: sid,
		Role:      anamnesia.WorkingToolOutput,
		Body:      body,
		Meta:      meta,
	}
	if err := d.Store.AppendWorking(r.Context(), entry); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       entry.ID,
		"position": entry.Position,
	})
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

func (d Deps) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
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
	sid, err := uuid.Parse(ev.SessionID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"folded_entries": 0, "skipped": "no session id"})
		return
	}
	entries, err := d.Store.RecallWorking(r.Context(), scope, sid, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(entries) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"folded_entries": 0})
		return
	}
	var sb strings.Builder
	for i, e := range entries {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(string(e.Role))
		sb.WriteString(": ")
		sb.WriteString(e.Body)
	}
	expBody := sb.String()
	var expPII []string
	if d.PII != nil {
		cleaned, tags, err := d.PII.Scrub(r.Context(), expBody)
		if err == nil {
			expBody = cleaned
			expPII = tags
		}
	}
	exp := &anamnesia.Experience{
		Scope:       scope,
		Kind:        anamnesia.ExperienceCase,
		Abstraction: 0,
		Body:        expBody,
		PIITags:     expPII,
		Meta: map[string]any{
			"session_id":  sid,
			"entry_count": len(entries),
			"folded_at":   time.Now().UTC(),
		},
	}
	if err := d.Store.RecordExperience(r.Context(), exp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n, err := d.Store.FoldWorking(r.Context(), scope, sid, exp.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"folded_entries": n,
		"experience_id":  exp.ID,
	})
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

// isReadOnlyTool reports whether tool is one we don't bother capturing.
var readOnlyTools = map[string]bool{
	"Read":     true,
	"LS":       true,
	"Glob":     true,
	"Grep":     true,
	"TodoWrite": true,
}

func isReadOnlyTool(name string) bool { return readOnlyTools[name] }

// summariseToolUse produces a short text body for the working-memory
// entry. We deliberately don't store the full input/response — that'd
// be very large and noisy for things like Bash output. Callers can
// recover detail from Meta.
func summariseToolUse(tool string, input, response map[string]any) string {
	if tool == "" {
		tool = "unknown"
	}
	switch tool {
	case "Bash":
		if cmd, _ := input["command"].(string); cmd != "" {
			return "Bash: " + truncate(cmd, 240)
		}
	case "Edit", "Write":
		if p, _ := input["file_path"].(string); p != "" {
			return tool + ": " + p
		}
	}
	if response != nil {
		if msg, _ := response["error"].(string); msg != "" {
			return tool + " (error): " + truncate(msg, 240)
		}
	}
	return tool
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
