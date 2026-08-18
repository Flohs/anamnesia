// Package retrieval implements hybrid (vector + lexical) search across
// facts, experiences, and skills, fused with reciprocal-rank fusion.
package retrieval

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/embed"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// Engine wires a Store + Embedder for query-time retrieval. Reranker is
// optional; when non-nil it re-orders the fused candidate list as the
// final step.
type Engine struct {
	Store    *store.Store
	Embedder embed.Embedder
	Reranker Reranker
}

// Query controls the search.
type Query struct {
	Scope     anamnesia.Scope
	Text      string
	Domains   []anamnesia.Domain // empty = all
	K         int                // top-K to return (default 10)
	VectorK   int                // vector candidates per domain (default 40)
	LexicalK  int                // lexical candidates per domain (default 40)
	RRFConst  float64            // default 60
	ProjectIn []uuid.UUID        // include hits from these projects; empty + ProjectID set = restrict to that project
	// OnlyRaw, when true, restricts experience retrieval to abstraction=0
	// rows — verbatim sources only, no consolidator-generated summaries.
	// Set this for evidence-grounded answering (benchmarks, citation
	// flows). Leave false for context injection where thematic summaries
	// are useful.
	OnlyRaw bool
	// Trace, when set, records the stages of this search: what was
	// searched for, what each half returned, how fusion ranked it and
	// what the reranker did to that order.
	//
	// Only the retrieve endpoint sets it. The extractor calls Search
	// twice per source, for its gate and its candidate fetch, and
	// tracing those would bury the real traces under parasitic ones.
	Trace *activity.Trace
}

// tracedRanking caps how many fused candidates a trace records. The
// recorder drops any single detail value over its size limit, so an
// uncapped ranking would silently vanish from the trace rather than
// merely be long.
const tracedRanking = 25

// Search runs vector + lexical retrieval per requested domain and fuses
// the results with reciprocal-rank fusion.
func (e *Engine) Search(ctx context.Context, q Query) ([]anamnesia.SearchHit, error) {
	if e == nil || e.Store == nil {
		return nil, errors.New("retrieval: engine not initialised")
	}
	if q.K <= 0 {
		q.K = 10
	}
	if q.VectorK <= 0 {
		q.VectorK = 40
	}
	if q.LexicalK <= 0 {
		q.LexicalK = 40
	}
	if q.RRFConst <= 0 {
		q.RRFConst = 60
	}
	if len(q.Domains) == 0 {
		q.Domains = []anamnesia.Domain{anamnesia.DomainFact, anamnesia.DomainExperience, anamnesia.DomainSkill}
	}

	if q.Trace != nil {
		q.Trace.Step("query", fmt.Sprintf("Retrieving for %q", q.Text), map[string]any{
			"prompt": q.Text,
			"scope":  scopeDetail(q.Scope),
			"limits": map[string]any{
				"k": q.K, "vector_k": q.VectorK, "lexical_k": q.LexicalK,
				"rrf_const": q.RRFConst, "only_raw": q.OnlyRaw,
			},
		})
	}

	// Embed query text. Skip if no embedder or empty text.
	var qvec []float32
	var embedErr error
	if e.Embedder != nil && strings.TrimSpace(q.Text) != "" {
		v, err := e.Embedder.Embed(ctx, []string{q.Text})
		embedErr = err
		if err == nil && len(v) > 0 {
			qvec = v[0]
		}
	}

	type ranked struct {
		hit anamnesia.SearchHit
		vRk int
		lRk int
	}
	byID := map[string]*ranked{}
	var vectorHits, lexicalHits []anamnesia.SearchHit

	add := func(domain anamnesia.Domain, items []anamnesia.SearchHit, vector bool) {
		for i, h := range items {
			h.Domain = domain
			if q.Trace != nil {
				if vector {
					vectorHits = append(vectorHits, h)
				} else {
					lexicalHits = append(lexicalHits, h)
				}
			}
			key := string(domain) + ":" + h.ID().String()
			r, ok := byID[key]
			if !ok {
				r = &ranked{hit: h}
				byID[key] = r
			}
			if vector {
				if r.vRk == 0 {
					r.vRk = i + 1
				}
			} else {
				if r.lRk == 0 {
					r.lRk = i + 1
				}
			}
		}
	}

	for _, d := range q.Domains {
		switch d {
		case anamnesia.DomainFact:
			if qvec != nil {
				hits, err := e.vectorFacts(ctx, q.Scope, qvec, q.VectorK)
				if err != nil {
					return nil, fmt.Errorf("vector facts: %w", err)
				}
				add(d, hits, true)
			}
			hits, err := e.lexicalFacts(ctx, q.Scope, q.Text, q.LexicalK)
			if err != nil {
				return nil, fmt.Errorf("lex facts: %w", err)
			}
			add(d, hits, false)
		case anamnesia.DomainExperience:
			if qvec != nil {
				hits, err := e.vectorExperiences(ctx, q.Scope, qvec, q.VectorK, q.OnlyRaw)
				if err != nil {
					return nil, fmt.Errorf("vector experiences: %w", err)
				}
				add(d, hits, true)
			}
			hits, err := e.lexicalExperiences(ctx, q.Scope, q.Text, q.LexicalK, q.OnlyRaw)
			if err != nil {
				return nil, fmt.Errorf("lex experiences: %w", err)
			}
			add(d, hits, false)
		case anamnesia.DomainSkill:
			hits, err := e.lexicalSkills(ctx, q.Scope, q.Text, q.LexicalK)
			if err != nil {
				return nil, fmt.Errorf("lex skills: %w", err)
			}
			add(d, hits, false)
		}
	}

	if q.Trace != nil {
		if qvec == nil {
			q.Trace.Step("vector", "No vector search ran", map[string]any{
				"skipped": true,
				"reason":  noVectorReason(e.Embedder != nil, q.Text, embedErr),
			})
		} else {
			q.Trace.Step("vector", fmt.Sprintf("%d vector hits", len(vectorHits)),
				map[string]any{"hits": HitDetails(vectorHits)})
		}
		q.Trace.Step("lexical", fmt.Sprintf("%d full-text hits", len(lexicalHits)),
			map[string]any{"hits": HitDetails(lexicalHits)})
	}

	out := make([]anamnesia.SearchHit, 0, len(byID))
	for _, r := range byID {
		score := 0.0
		if r.vRk > 0 {
			score += 1.0 / (q.RRFConst + float64(r.vRk))
		}
		if r.lRk > 0 {
			score += 1.0 / (q.RRFConst + float64(r.lRk))
		}
		// Decay-aware boost: experiences carry a precomputed relevance
		// (recency + frequency × importance). Multiply rather than add
		// so a stale row never outranks a fresher one with the same
		// retrieval signal.
		if r.hit.Domain == anamnesia.DomainExperience && r.hit.Experience != nil {
			rel := float64(r.hit.Experience.Relevance)
			if rel <= 0 {
				rel = 0.1
			}
			score *= rel
		}
		r.hit.Score = score
		r.hit.VectorRank = r.vRk
		r.hit.LexicalRank = r.lRk
		out = append(out, r.hit)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })

	if q.Trace != nil {
		q.Trace.Step("fuse", fmt.Sprintf("RRF fused %d candidates", len(out)),
			map[string]any{"ranked": rankedDetails(out)})
	}

	// Take a candidate set 4× the requested K into the reranker so the
	// final ordering has room to reshuffle. If no reranker is wired,
	// just cap at K.
	candK := q.K
	if e.Reranker != nil {
		candK = 4 * q.K
	}
	if len(out) > candK {
		out = out[:candK]
	}
	before := order(out)
	applied := false
	var rerankErr error
	if e.Reranker != nil && strings.TrimSpace(q.Text) != "" {
		reranked, err := e.Reranker.Rerank(ctx, q.Text, out)
		if err == nil {
			out = reranked
			applied = true
		} else {
			rerankErr = err
		}
	}
	if q.Trace != nil {
		detail := map[string]any{
			"applied": applied,
			"before":  before,
			"after":   order(out),
		}
		if e.Reranker == nil {
			detail["reason"] = "no reranker is configured"
		}
		if rerankErr != nil {
			detail["error"] = rerankErr.Error()
		}
		q.Trace.Step("rerank", rerankSummary(applied, before, order(out), rerankErr), detail)
	}
	if len(out) > q.K {
		out = out[:q.K]
	}
	return out, nil
}

func (e *Engine) vectorFacts(ctx context.Context, scope anamnesia.Scope, qvec []float32, k int) ([]anamnesia.SearchHit, error) {
	args := []any{scope.UserID, pgvector.NewVector(qvec)}
	where := []string{"user_id = $1", "deleted_at IS NULL", "embedding IS NOT NULL"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("(project_id = $%d OR project_id IS NULL)", len(args)))
	}
	args = append(args, k)
	q := fmt.Sprintf(`
		SELECT id, user_id, project_id, source_id, fact_scope, key, value, source, trust, pii_tags,
		       embed_model, valid_from, valid_to, ingested_at, invalidated_at,
		       superseded_by, deleted_at
		FROM facts WHERE %s
		ORDER BY embedding <=> $2 ASC
		LIMIT $%d`, strings.Join(where, " AND "), len(args))
	return e.scanFactHits(ctx, q, args)
}

func (e *Engine) lexicalFacts(ctx context.Context, scope anamnesia.Scope, text string, k int) ([]anamnesia.SearchHit, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	args := []any{scope.UserID, text}
	where := []string{"user_id = $1", "deleted_at IS NULL", "tsv @@ plainto_tsquery('english', $2)"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("(project_id = $%d OR project_id IS NULL)", len(args)))
	}
	args = append(args, k)
	q := fmt.Sprintf(`
		SELECT id, user_id, project_id, source_id, fact_scope, key, value, source, trust, pii_tags,
		       embed_model, valid_from, valid_to, ingested_at, invalidated_at,
		       superseded_by, deleted_at
		FROM facts WHERE %s
		ORDER BY ts_rank_cd(tsv, plainto_tsquery('english', $2)) DESC
		LIMIT $%d`, strings.Join(where, " AND "), len(args))
	return e.scanFactHits(ctx, q, args)
}

func (e *Engine) scanFactHits(ctx context.Context, q string, args []any) ([]anamnesia.SearchHit, error) {
	facts, err := e.Store.QueryFacts(ctx, q, args)
	if err != nil {
		return nil, err
	}
	out := make([]anamnesia.SearchHit, len(facts))
	for i, f := range facts {
		out[i] = anamnesia.SearchHit{Domain: anamnesia.DomainFact, Fact: f}
	}
	return out, nil
}

func (e *Engine) vectorExperiences(ctx context.Context, scope anamnesia.Scope, qvec []float32, k int, onlyRaw bool) ([]anamnesia.SearchHit, error) {
	args := []any{scope.UserID, pgvector.NewVector(qvec)}
	where := []string{"user_id = $1", "deleted_at IS NULL", "invalidated_at IS NULL", "embedding IS NOT NULL"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("(project_id = $%d OR project_id IS NULL)", len(args)))
	}
	if onlyRaw {
		where = append(where, "abstraction = 0")
	}
	args = append(args, k)
	q := fmt.Sprintf(`SELECT id, user_id, project_id, source_id, kind, abstraction, title, body, outcome, meta,
		trust, importance, relevance, pii_tags, use_count, last_used_at, embed_model,
		valid_from, valid_to, ingested_at, invalidated_at, superseded_by, deleted_at,
		occurred_at, participants, topic, parent_id, provenance
		FROM experiences WHERE %s ORDER BY embedding <=> $2 ASC LIMIT $%d`,
		strings.Join(where, " AND "), len(args))
	return e.scanExperienceHits(ctx, q, args)
}

func (e *Engine) lexicalExperiences(ctx context.Context, scope anamnesia.Scope, text string, k int, onlyRaw bool) ([]anamnesia.SearchHit, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	args := []any{scope.UserID, text}
	where := []string{"user_id = $1", "deleted_at IS NULL", "invalidated_at IS NULL", "tsv @@ plainto_tsquery('english', $2)"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("(project_id = $%d OR project_id IS NULL)", len(args)))
	}
	if onlyRaw {
		where = append(where, "abstraction = 0")
	}
	args = append(args, k)
	q := fmt.Sprintf(`SELECT id, user_id, project_id, source_id, kind, abstraction, title, body, outcome, meta,
		trust, importance, relevance, pii_tags, use_count, last_used_at, embed_model,
		valid_from, valid_to, ingested_at, invalidated_at, superseded_by, deleted_at,
		occurred_at, participants, topic, parent_id, provenance
		FROM experiences WHERE %s ORDER BY ts_rank_cd(tsv, plainto_tsquery('english', $2)) DESC LIMIT $%d`,
		strings.Join(where, " AND "), len(args))
	return e.scanExperienceHits(ctx, q, args)
}

func (e *Engine) scanExperienceHits(ctx context.Context, q string, args []any) ([]anamnesia.SearchHit, error) {
	exps, err := e.Store.QueryExperiences(ctx, q, args)
	if err != nil {
		return nil, err
	}
	out := make([]anamnesia.SearchHit, len(exps))
	for i, x := range exps {
		out[i] = anamnesia.SearchHit{Domain: anamnesia.DomainExperience, Experience: x}
	}
	return out, nil
}

func (e *Engine) lexicalSkills(ctx context.Context, scope anamnesia.Scope, text string, k int) ([]anamnesia.SearchHit, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	// Skills don't have a tsvector column; use simple ILIKE matching.
	args := []any{scope.UserID, "%" + text + "%"}
	where := []string{"user_id = $1", "deleted_at IS NULL", "(name ILIKE $2 OR description ILIKE $2)"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("(project_id = $%d OR project_id IS NULL)", len(args)))
	}
	args = append(args, k)
	q := fmt.Sprintf(`SELECT id, user_id, project_id, name, kind, description, signature, body, meta,
		use_count, last_used_at FROM skills WHERE %s ORDER BY use_count DESC, name ASC LIMIT $%d`,
		strings.Join(where, " AND "), len(args))
	skills, err := e.Store.QuerySkills(ctx, q, args)
	if err != nil {
		return nil, err
	}
	out := make([]anamnesia.SearchHit, len(skills))
	for i, sk := range skills {
		out[i] = anamnesia.SearchHit{Domain: anamnesia.DomainSkill, Skill: sk}
	}
	return out, nil
}

// ─── trace detail ────────────────────────────────────────────────────
//
// These render search internals for a trace step. They are only built
// when a trace is attached, so a search nobody is watching pays nothing
// for them.

func scopeDetail(scope anamnesia.Scope) map[string]any {
	d := map[string]any{"user_id": scope.UserID.String()}
	if scope.ProjectID != nil {
		d["project_id"] = scope.ProjectID.String()
	}
	return d
}

func noVectorReason(hasEmbedder bool, text string, err error) string {
	switch {
	case err != nil:
		return "embedding the query failed: " + err.Error()
	case !hasEmbedder:
		return "no embedder is configured, so nothing is embedded and vector search cannot run"
	case strings.TrimSpace(text) == "":
		return "the query carries no text to embed"
	}
	return "the embedder returned no vector"
}

func hitTitle(h anamnesia.SearchHit) string {
	switch h.Domain {
	case anamnesia.DomainFact:
		if h.Fact != nil {
			return h.Fact.Key
		}
	case anamnesia.DomainExperience:
		if h.Experience != nil {
			if h.Experience.Title != "" {
				return h.Experience.Title
			}
			body := h.Experience.Body
			if i := strings.IndexByte(body, '\n'); i >= 0 {
				body = body[:i]
			}
			if len(body) > 120 {
				body = body[:120]
			}
			return body
		}
	case anamnesia.DomainSkill:
		if h.Skill != nil {
			return h.Skill.Name
		}
	}
	return ""
}

// HitDetails renders search hits for a trace step: enough to see what
// was found, without the row itself. Exported because the extractor and
// the HTTP layer show the same thing.
func HitDetails(hits []anamnesia.SearchHit) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, map[string]any{
			"id":    h.ID().String(),
			"kind":  string(h.Domain),
			"title": hitTitle(h),
			"score": h.Score,
		})
	}
	return out
}

// rankedDetails renders the fused ranking, which is the one thing that
// makes rank movement legible. Capped, and it says when it capped.
func rankedDetails(hits []anamnesia.SearchHit) []map[string]any {
	n := len(hits)
	capped := false
	if n > tracedRanking {
		n, capped = tracedRanking, true
	}
	out := make([]map[string]any, 0, n+1)
	for _, h := range hits[:n] {
		out = append(out, map[string]any{
			"id":           h.ID().String(),
			"kind":         string(h.Domain),
			"title":        hitTitle(h),
			"rrf_score":    h.Score,
			"vector_rank":  h.VectorRank,
			"lexical_rank": h.LexicalRank,
		})
	}
	if capped {
		out = append(out, map[string]any{"capped_after": tracedRanking, "of": len(hits)})
	}
	return out
}

// order is the id sequence of a result list, which is all the rerank
// step needs to show what moved.
func order(hits []anamnesia.SearchHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ID().String())
	}
	return out
}

func rerankSummary(applied bool, before, after []string, err error) string {
	switch {
	case err != nil:
		return "Rerank failed, keeping the fused order"
	case !applied:
		return "No rerank ran, so the fused order stands"
	case len(before) == 0 || len(after) == 0:
		return "Reranked an empty candidate set"
	case before[0] == after[0]:
		return "Reranked, top result unchanged"
	}
	return "Reranked, a new top result"
}
