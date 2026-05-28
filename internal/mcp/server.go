// Package mcp exposes the Anamnesia memory model as MCP tools. The
// server is mounted at /mcp by the HTTP layer; Claude Code (or any
// streamable-http MCP client) connects directly to it.
//
// Tool scoping: every tool accepts optional `user` and `project` slug
// parameters. If absent, the server falls back to the configured
// defaults so a single-user install can just work without arguments.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/flohs/anamnesia-open-source/internal/jobs"
	"github.com/flohs/anamnesia-open-source/internal/pii"
	"github.com/flohs/anamnesia-open-source/internal/retrieval"
	"github.com/flohs/anamnesia-open-source/internal/store"
	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

// Deps wires the MCP server.
type Deps struct {
	Store          *store.Store
	Retrieval      *retrieval.Engine
	PII            pii.Detector
	Briefer        *jobs.Briefer // nil-safe
	DefaultUser    string
	DefaultProject string
}

// NewHandler builds the MCP HTTP handler.
func NewHandler(d Deps) http.Handler {
	s := server.NewMCPServer(
		"anamnesia",
		"0.1.0",
		server.WithToolCapabilities(false),
	)
	registerTools(s, d)
	return server.NewStreamableHTTPServer(s)
}

// ─── tool registration ────────────────────────────────────────────────

func registerTools(s *server.MCPServer, d Deps) {
	s.AddTool(
		mcp.NewTool("anamnesia_facts_upsert",
			mcp.WithDescription("Upsert a fact in the memory store. Identity = (user, project, scope, key)."),
			mcp.WithString("key", mcp.Required(), mcp.Description("identifier for the fact")),
			mcp.WithObject("value", mcp.Required(), mcp.Description("the fact body (free JSON object)")),
			mcp.WithString("scope", mcp.Description("user|project|environment (default: project)")),
			mcp.WithString("user", mcp.Description("override default user")),
			mcp.WithString("project", mcp.Description("override default project")),
			mcp.WithString("source", mcp.Description("where the fact came from")),
			mcp.WithNumber("trust", mcp.Description("0..1 confidence (default 0.5)")),
		),
		d.factsUpsert,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_facts_list",
			mcp.WithDescription("List facts in scope, newest first."),
			mcp.WithString("user"), mcp.WithString("project"),
			mcp.WithString("fact_scope", mcp.Description("filter to user|project|environment")),
			mcp.WithNumber("limit"),
		),
		d.factsList,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_facts_forget",
			mcp.WithDescription("Soft-delete a fact by id."),
			mcp.WithString("id", mcp.Required()),
		),
		d.factsForget,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_experience_record",
			mcp.WithDescription("Append an experience (trajectory, strategy, insight)."),
			mcp.WithString("body", mcp.Required(), mcp.Description("free-text body")),
			mcp.WithString("kind", mcp.Description("case|strategy|hybrid")),
			mcp.WithString("title"),
			mcp.WithString("outcome", mcp.Description("success|failure|partial")),
			mcp.WithObject("meta"),
			mcp.WithString("user"), mcp.WithString("project"),
		),
		d.experienceRecord,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_experience_supersede",
			mcp.WithDescription("Mark experience old_id as superseded by new_id."),
			mcp.WithString("old_id", mcp.Required()),
			mcp.WithString("new_id", mcp.Required()),
		),
		d.experienceSupersede,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_experience_forget",
			mcp.WithDescription("Soft-delete an experience by id."),
			mcp.WithString("id", mcp.Required()),
		),
		d.experienceForget,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_search",
			mcp.WithDescription("Hybrid (vector + lexical) search across facts/experiences/skills."),
			mcp.WithString("text", mcp.Required()),
			mcp.WithNumber("k"),
			mcp.WithString("user"), mcp.WithString("project"),
		),
		d.search,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_identity",
			mcp.WithDescription("Return the user's identity: persona + profile + a rendered system_prompt block. Dock-on agents call this at boot."),
			mcp.WithString("user"), mcp.WithString("project"),
		),
		d.identity,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_commitments_record",
			mcp.WithDescription("Record an open commitment (something owed by/to the user). Status defaults to 'open'."),
			mcp.WithString("body", mcp.Required()),
			mcp.WithString("owner", mcp.Description("party owing — default 'user'")),
			mcp.WithString("beneficiary", mcp.Description("party owed — default 'user'")),
			mcp.WithString("due_at", mcp.Description("RFC3339; optional")),
			mcp.WithString("user"), mcp.WithString("project"),
		),
		d.commitmentRecord,
	)
	s.AddTool(
		mcp.NewTool("anamnesia_commitments_list",
			mcp.WithDescription("List commitments. Default sort: open first, then by due date."),
			mcp.WithString("status", mcp.Description("open|done|dropped — default any")),
			mcp.WithNumber("limit"),
			mcp.WithString("user"), mcp.WithString("project"),
		),
		d.commitmentList,
	)
	s.AddTool(
		mcp.NewTool("anamnesia_commitments_resolve",
			mcp.WithDescription("Mark a commitment done or dropped."),
			mcp.WithString("id", mcp.Required()),
			mcp.WithString("status", mcp.Required(), mcp.Description("done|dropped")),
		),
		d.commitmentResolve,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_capabilities",
			mcp.WithDescription("List skills/tools registered for this user, freshness-ordered. Use at boot to discover what you can call."),
			mcp.WithString("user"), mcp.WithString("project"),
			mcp.WithNumber("limit"),
		),
		d.capabilities,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_people",
			mcp.WithDescription("List people the user knows, sorted by recent-mention count (last 90d)."),
			mcp.WithString("user"), mcp.WithString("project"),
			mcp.WithNumber("limit"),
		),
		d.people,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_briefing",
			mcp.WithDescription("Summarise experiences in a time window with 'adjacent' items the user might want to mention. Returns {summary, highlights[], adjacent[]}."),
			mcp.WithString("since", mcp.Required(), mcp.Description("RFC3339 timestamp")),
			mcp.WithString("until", mcp.Description("RFC3339; default now")),
			mcp.WithString("topic"),
			mcp.WithNumber("max_adjacent"),
			mcp.WithString("user"), mcp.WithString("project"),
		),
		d.briefing,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_skills_register",
			mcp.WithDescription("Register or update a callable in the skills registry."),
			mcp.WithString("name", mcp.Required()),
			mcp.WithString("kind", mcp.Description("function|script|api|mcp")),
			mcp.WithString("description"),
			mcp.WithObject("signature"),
			mcp.WithString("body"),
			mcp.WithObject("meta"),
			mcp.WithString("user"), mcp.WithString("project"),
		),
		d.skillsRegister,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_skills_list",
			mcp.WithDescription("List skills in scope."),
			mcp.WithString("user"), mcp.WithString("project"),
			mcp.WithNumber("limit"),
		),
		d.skillsList,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_working_append",
			mcp.WithDescription("Append an entry to working memory for the given session."),
			mcp.WithString("session_id", mcp.Required()),
			mcp.WithString("body", mcp.Required()),
			mcp.WithString("role", mcp.Description("observation|plan|state|tool_output")),
			mcp.WithObject("meta"),
			mcp.WithString("user"), mcp.WithString("project"),
		),
		d.workingAppend,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_working_recall",
			mcp.WithDescription("Recall the working-memory trail for a session."),
			mcp.WithString("session_id", mcp.Required()),
			mcp.WithString("user"), mcp.WithString("project"),
			mcp.WithNumber("limit"),
		),
		d.workingRecall,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_audit",
			mcp.WithDescription("Audit log: tail (default) or per-subject history if subject is provided."),
			mcp.WithString("subject", mcp.Description(`"<kind>:<uuid>" e.g. "fact:abc..."; if set, returns rows for that subject only`)),
			mcp.WithString("user"), mcp.WithString("project"),
			mcp.WithNumber("limit"),
		),
		d.audit,
	)

	// ─── graph: entities + edges + neighbors ───────────────────────────
	s.AddTool(
		mcp.NewTool("anamnesia_graph_entity",
			mcp.WithDescription("Upsert a graph entity. Identity = (scope, kind, name)."),
			mcp.WithString("kind", mcp.Required(), mcp.Description("e.g. person|concept|file|tool")),
			mcp.WithString("name", mcp.Required()),
			mcp.WithObject("props"),
			mcp.WithString("user"), mcp.WithString("project"),
		),
		d.graphEntity,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_graph_edge",
			mcp.WithDescription("Create a typed bitemporal edge between two entities."),
			mcp.WithString("from_id", mcp.Required()),
			mcp.WithString("to_id", mcp.Required()),
			mcp.WithString("kind", mcp.Required(), mcp.Description("relation type, e.g. prefers|worked_on")),
			mcp.WithObject("props"),
			mcp.WithString("source"),
			mcp.WithNumber("trust"),
		),
		d.graphEdge,
	)

	s.AddTool(
		mcp.NewTool("anamnesia_graph_neighbors",
			mcp.WithDescription("Return entities reachable from src via currently-valid edges."),
			mcp.WithString("entity_id", mcp.Required()),
			mcp.WithString("direction", mcp.Description("out|in|both (default out)")),
			mcp.WithArray("kinds", mcp.Description("filter to these edge kinds")),
			mcp.WithNumber("limit"),
		),
		d.graphNeighbors,
	)

	// ─── ingest: generic content → extractor pipeline ──────────────────
	s.AddTool(
		mcp.NewTool("anamnesia_ingest",
			mcp.WithDescription(
				"Push a piece of content into the memory system. The extractor "+
					"reads it asynchronously and decides what (if anything) to "+
					"persist as facts or experiences. Use for meeting transcripts, "+
					"notes, emails, anything the user wants Anamnesia to learn "+
					"from. Returns a source_id; use anamnesia_audit later to see "+
					"what ops were produced."),
			mcp.WithString("content", mcp.Required(), mcp.Description("raw text to consider")),
			mcp.WithString("kind", mcp.Description("source kind hint: chat-turn|transcript|document|note|email|calendar|tool-output (default: note)")),
			mcp.WithString("title"),
			mcp.WithString("external_ref", mcp.Description("optional external identifier (e.g. meeting URL)")),
			mcp.WithArray("participants"),
			mcp.WithBoolean("preserve_raw", mcp.Description("if true, the raw text is kept past the TTL")),
			mcp.WithString("user"), mcp.WithString("project"),
		),
		d.ingest,
	)
}

// ─── helpers ─────────────────────────────────────────────────────────

func (d Deps) resolveScope(ctx context.Context, args map[string]any) (anamnesia.Scope, error) {
	user, _ := args["user"].(string)
	if user == "" {
		user = d.DefaultUser
	}
	if user == "" {
		user = "default"
	}
	uid, err := d.Store.EnsureUser(ctx, user)
	if err != nil {
		return anamnesia.Scope{}, err
	}
	scope := anamnesia.Scope{UserID: uid}
	proj, _ := args["project"].(string)
	if proj == "" {
		proj = d.DefaultProject
	}
	if proj != "" {
		pid, err := d.Store.EnsureProject(ctx, uid, proj)
		if err != nil {
			return anamnesia.Scope{}, err
		}
		scope.ProjectID = &pid
	}
	return scope, nil
}

func argsFromRequest(req mcp.CallToolRequest) map[string]any {
	args := req.GetArguments()
	if args == nil {
		return map[string]any{}
	}
	return args
}

func ok(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

func bad(err error) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(err.Error()), nil
}

func argInt(m map[string]any, key string, def int) int {
	if v, ok := m[key]; ok {
		switch t := v.(type) {
		case float64:
			return int(t)
		case int:
			return t
		}
	}
	return def
}

func argString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func argFloat32(m map[string]any, key string, def float32) float32 {
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return float32(f)
		}
	}
	return def
}

func argMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func argUUID(m map[string]any, key string) (uuid.UUID, error) {
	s := argString(m, key)
	if s == "" {
		return uuid.Nil, fmt.Errorf("%s required", key)
	}
	return uuid.Parse(s)
}

// ─── tool handlers ───────────────────────────────────────────────────

func (d Deps) factsUpsert(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	key := argString(args, "key")
	if key == "" {
		return bad(errors.New("key required"))
	}
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	fs := anamnesia.FactScope(argString(args, "scope"))
	if !fs.Valid() {
		fs = anamnesia.FactScopeProject
	}
	value := argMap(args, "value")
	if value == nil {
		value = map[string]any{}
	}
	f := &anamnesia.Fact{
		Scope:    scope,
		Key:      key,
		Value:    value,
		FactKind: fs,
		Source:   argString(args, "source"),
		Trust:    argFloat32(args, "trust", 0.5),
	}
	if err := d.Store.UpsertFact(ctx, f); err != nil {
		return bad(err)
	}
	return ok(f)
}

func (d Deps) factsList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	fs := anamnesia.FactScope(argString(args, "fact_scope"))
	limit := argInt(args, "limit", 50)
	out, err := d.Store.ListFacts(ctx, scope, fs, limit)
	if err != nil {
		return bad(err)
	}
	return ok(out)
}

func (d Deps) factsForget(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	id, err := argUUID(args, "id")
	if err != nil {
		return bad(err)
	}
	if err := d.Store.ForgetFact(ctx, id); err != nil {
		return bad(err)
	}
	return ok(map[string]any{"id": id, "forgotten": true})
}

func (d Deps) experienceRecord(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	body := argString(args, "body")
	if body == "" {
		return bad(errors.New("body required"))
	}
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	kind := anamnesia.ExperienceKind(argString(args, "kind"))
	if !kind.Valid() {
		kind = anamnesia.ExperienceCase
	}
	var piiTags []string
	if d.PII != nil {
		cleaned, tags, err := d.PII.Scrub(ctx, body)
		if err == nil {
			body = cleaned
			piiTags = tags
		}
	}
	e := &anamnesia.Experience{
		Scope:   scope,
		Kind:    kind,
		Title:   argString(args, "title"),
		Body:    body,
		PIITags: piiTags,
		Outcome: anamnesia.Outcome(argString(args, "outcome")),
		Meta:    argMap(args, "meta"),
	}
	if err := d.Store.RecordExperience(ctx, e); err != nil {
		return bad(err)
	}
	return ok(e)
}

func (d Deps) experienceSupersede(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	oldID, err := argUUID(args, "old_id")
	if err != nil {
		return bad(err)
	}
	newID, err := argUUID(args, "new_id")
	if err != nil {
		return bad(err)
	}
	if err := d.Store.SupersedeExperience(ctx, oldID, newID); err != nil {
		return bad(err)
	}
	return ok(map[string]any{"old_id": oldID, "new_id": newID})
}

func (d Deps) experienceForget(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	id, err := argUUID(args, "id")
	if err != nil {
		return bad(err)
	}
	if err := d.Store.ForgetExperience(ctx, id); err != nil {
		return bad(err)
	}
	return ok(map[string]any{"id": id, "forgotten": true})
}

func (d Deps) identity(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	id, err := d.Store.GetIdentity(ctx, scope)
	if err != nil {
		return bad(err)
	}
	return ok(id)
}

func (d Deps) commitmentRecord(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	c := &anamnesia.Commitment{
		Scope: scope, Owner: argString(args, "owner"),
		Beneficiary: argString(args, "beneficiary"), Body: argString(args, "body"),
	}
	if s := argString(args, "due_at"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return bad(fmt.Errorf("due_at: %w", err))
		}
		c.DueAt = &t
	}
	if err := d.Store.RecordCommitment(ctx, c); err != nil {
		return bad(err)
	}
	return ok(c)
}

func (d Deps) commitmentList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	out, err := d.Store.ListCommitments(ctx, scope,
		anamnesia.CommitmentStatus(argString(args, "status")), argInt(args, "limit", 50))
	if err != nil {
		return bad(err)
	}
	return ok(out)
}

func (d Deps) commitmentResolve(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	id, err := argUUID(args, "id")
	if err != nil {
		return bad(err)
	}
	st := anamnesia.CommitmentStatus(argString(args, "status"))
	if err := d.Store.ResolveCommitment(ctx, id, st); err != nil {
		return bad(err)
	}
	return ok(map[string]any{"id": id, "status": st})
}

func (d Deps) capabilities(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	out, err := d.Store.ListCapabilities(ctx, scope, argInt(args, "limit", 50))
	if err != nil {
		return bad(err)
	}
	return ok(out)
}

func (d Deps) people(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	out, err := d.Store.ListPeople(ctx, scope, argInt(args, "limit", 50))
	if err != nil {
		return bad(err)
	}
	return ok(out)
}

func (d Deps) briefing(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	since, err := time.Parse(time.RFC3339, argString(args, "since"))
	if err != nil {
		return bad(fmt.Errorf("since: %w", err))
	}
	var until time.Time
	if u := argString(args, "until"); u != "" {
		until, err = time.Parse(time.RFC3339, u)
		if err != nil {
			return bad(fmt.Errorf("until: %w", err))
		}
	}
	topic := argString(args, "topic")
	inWin, err := d.Store.ListExperiencesInWindow(ctx, scope, since, until, topic, 200)
	if err != nil {
		return bad(err)
	}
	flankSince := since.AddDate(0, 0, -14)
	flankUntil := until
	if flankUntil.IsZero() {
		flankUntil = time.Now().UTC()
	}
	flankUntil = flankUntil.AddDate(0, 0, 14)
	adj, err := d.Store.ListExperiencesInWindow(ctx, scope, flankSince, flankUntil, topic, 50)
	if err != nil {
		return bad(err)
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
	out, err := briefer.Brief(ctx, anamnesia.BriefingRequest{
		Scope: scope, Since: since, Until: until,
		Topic: topic, MaxAdjacent: argInt(args, "max_adjacent", 3),
	}, inWin, adjOut)
	if err != nil {
		return bad(err)
	}
	return ok(out)
}

func (d Deps) search(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	text := argString(args, "text")
	if text == "" {
		return bad(errors.New("text required"))
	}
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	hits, err := d.Retrieval.Search(ctx, retrieval.Query{
		Scope: scope, Text: text, K: argInt(args, "k", 10),
	})
	if err != nil {
		return bad(err)
	}
	return ok(hits)
}

func (d Deps) skillsRegister(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	name := argString(args, "name")
	if name == "" {
		return bad(errors.New("name required"))
	}
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	kind := anamnesia.SkillKind(argString(args, "kind"))
	if !kind.Valid() {
		kind = anamnesia.SkillFunction
	}
	sk := &anamnesia.Skill{
		Scope:       scope,
		Name:        name,
		Kind:        kind,
		Description: argString(args, "description"),
		Signature:   argMap(args, "signature"),
		Body:        argString(args, "body"),
		Meta:        argMap(args, "meta"),
	}
	if err := d.Store.RegisterSkill(ctx, sk); err != nil {
		return bad(err)
	}
	return ok(sk)
}

func (d Deps) skillsList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	out, err := d.Store.ListSkills(ctx, scope, argInt(args, "limit", 50))
	if err != nil {
		return bad(err)
	}
	return ok(out)
}

func (d Deps) workingAppend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	sid, err := argUUID(args, "session_id")
	if err != nil {
		return bad(err)
	}
	body := argString(args, "body")
	if body == "" {
		return bad(errors.New("body required"))
	}
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	role := anamnesia.WorkingRole(argString(args, "role"))
	if !role.Valid() {
		role = anamnesia.WorkingObservation
	}
	w := &anamnesia.WorkingEntry{
		Scope: scope, SessionID: sid, Role: role, Body: body,
		Meta: argMap(args, "meta"),
	}
	if err := d.Store.AppendWorking(ctx, w); err != nil {
		return bad(err)
	}
	return ok(w)
}

func (d Deps) workingRecall(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	sid, err := argUUID(args, "session_id")
	if err != nil {
		return bad(err)
	}
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	out, err := d.Store.RecallWorking(ctx, scope, sid, argInt(args, "limit", 0))
	if err != nil {
		return bad(err)
	}
	return ok(out)
}

func (d Deps) audit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	limit := argInt(args, "limit", 50)
	if subject := argString(args, "subject"); subject != "" {
		parts := strings.SplitN(subject, ":", 2)
		if len(parts) != 2 {
			return bad(errors.New(`subject must be "<kind>:<uuid>"`))
		}
		id, err := uuid.Parse(parts[1])
		if err != nil {
			return bad(err)
		}
		out, err := d.Store.AuditForSubject(ctx, parts[0], id, limit)
		if err != nil {
			return bad(err)
		}
		return ok(out)
	}
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	out, err := d.Store.AuditTail(ctx, scope, limit)
	if err != nil {
		return bad(err)
	}
	return ok(out)
}

// ─── graph handlers ──────────────────────────────────────────────────

func (d Deps) graphEntity(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	kind := argString(args, "kind")
	name := argString(args, "name")
	if kind == "" || name == "" {
		return bad(errors.New("kind and name required"))
	}
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	ent := &anamnesia.Entity{
		Scope: scope,
		Kind:  kind,
		Name:  name,
		Props: argMap(args, "props"),
	}
	if err := d.Store.UpsertEntity(ctx, ent); err != nil {
		return bad(err)
	}
	return ok(ent)
}

func (d Deps) graphEdge(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	from, err := argUUID(args, "from_id")
	if err != nil {
		return bad(err)
	}
	to, err := argUUID(args, "to_id")
	if err != nil {
		return bad(err)
	}
	kind := argString(args, "kind")
	if kind == "" {
		return bad(errors.New("kind required"))
	}
	edge := &anamnesia.Edge{
		From:   from,
		To:     to,
		Kind:   kind,
		Props:  argMap(args, "props"),
		Source: argString(args, "source"),
		Trust:  argFloat32(args, "trust", 0.5),
	}
	if err := d.Store.CreateEdge(ctx, edge); err != nil {
		return bad(err)
	}
	return ok(edge)
}

func (d Deps) graphNeighbors(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	id, err := argUUID(args, "entity_id")
	if err != nil {
		return bad(err)
	}
	dir := argString(args, "direction")
	if dir == "" {
		dir = "out"
	}
	kinds := argStringSlice(args, "kinds")
	ents, edges, err := d.Store.Neighbors(ctx, id, kinds, dir, argInt(args, "limit", 50))
	if err != nil {
		return bad(err)
	}
	return ok(map[string]any{"entities": ents, "edges": edges})
}

// ─── ingest handler ──────────────────────────────────────────────────

func (d Deps) ingest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := argsFromRequest(req)
	content := argString(args, "content")
	if content == "" {
		return bad(errors.New("content required"))
	}
	scope, err := d.resolveScope(ctx, args)
	if err != nil {
		return bad(err)
	}
	kind := argString(args, "kind")
	if kind == "" {
		kind = "note"
	}
	// PII scrub before persisting, same as the HTTP endpoint.
	if d.PII != nil {
		cleaned, _, err := d.PII.Scrub(ctx, content)
		if err == nil {
			content = cleaned
		}
	}
	src := &anamnesia.Source{
		Scope:        scope,
		Kind:         kind,
		Title:        argString(args, "title"),
		ExternalRef:  argString(args, "external_ref"),
		Participants: argStringSlice(args, "participants"),
		RawContent:   content,
		PreserveRaw:  argBool(args, "preserve_raw", false),
	}
	if err := d.Store.InsertSource(ctx, src); err != nil {
		return bad(err)
	}
	return ok(map[string]any{
		"source_id":  src.ID,
		"queued":     true,
		"expires_at": src.ExpiresAt,
	})
}

func argBool(m map[string]any, key string, def bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return def
}

func argStringSlice(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok {
		return nil
	}
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
