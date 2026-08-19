// Neighbour expansion: the third candidate channel, alongside vector and
// lexical search. It walks the entity graph out from the fused ranking's
// own top hits, so it only ever adds rows retrieval already has reason to
// trust the neighbourhood of — see Engine.Search for how these candidates
// get folded back into the RRF score.
package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// graphExpand walks out from the top q.GraphSeedN hits of fused (the
// vector+lexical fused ranking, already sorted) and returns up to
// q.GraphK extra candidate hits reachable by one edge hop, ordered by
// the trust of the edge that reached them:
//
//  1. collect the seed hits' source_ids
//  2. EntitiesForSources → the entities those sources mention
//  3. Neighbors(entity, nil, "both", GraphFanout) for each, restricted
//     to edges valid now
//  4. SourcesForEntities on the neighbours → their sources
//  5. facts and experiences from those sources, in edge-trust order
//
// Cheap no-op whenever the graph has nothing to say about these hits —
// which is every install today: step 2 finds no entities and this
// returns immediately, before any further query.
func (e *Engine) graphExpand(ctx context.Context, q Query, fused []anamnesia.SearchHit) ([]anamnesia.SearchHit, error) {
	seedN := q.GraphSeedN
	if seedN <= 0 || len(fused) == 0 {
		return nil, nil
	}
	if seedN > len(fused) {
		seedN = len(fused)
	}
	seeds := fused[:seedN]

	wantFacts, wantExperiences := false, false
	for _, d := range q.Domains {
		switch d {
		case anamnesia.DomainFact:
			wantFacts = true
		case anamnesia.DomainExperience:
			wantExperiences = true
		}
	}
	if !wantFacts && !wantExperiences {
		return nil, nil
	}

	seedSources := map[uuid.UUID]bool{}
	var sourceIDs []uuid.UUID
	for _, h := range seeds {
		id := hitSourceID(h)
		if id == nil || seedSources[*id] {
			continue
		}
		seedSources[*id] = true
		sourceIDs = append(sourceIDs, *id)
	}
	if len(sourceIDs) == 0 {
		return nil, nil
	}

	entities, err := e.Store.EntitiesForSources(ctx, sourceIDs)
	if err != nil {
		return nil, fmt.Errorf("entities for sources: %w", err)
	}
	if len(entities) == 0 {
		return nil, nil // empty graph: nothing mentions these sources
	}

	// Walk out from every seed entity, both directions, keeping the
	// highest-trust edge that reaches each neighbour.
	bestTrust := map[uuid.UUID]float32{}
	for _, ent := range entities {
		neighbors, edges, err := e.Store.Neighbors(ctx, ent.ID, nil, "both", q.GraphFanout)
		if err != nil {
			return nil, fmt.Errorf("neighbors: %w", err)
		}
		for i, n := range neighbors {
			if t := edges[i].Trust; t > bestTrust[n.ID] {
				bestTrust[n.ID] = t
			}
		}
	}
	if len(bestTrust) == 0 {
		return nil, nil
	}
	type neighbour struct {
		id    uuid.UUID
		trust float32
	}
	ranked := make([]neighbour, 0, len(bestTrust))
	for id, t := range bestTrust {
		ranked = append(ranked, neighbour{id, t})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].trust > ranked[j].trust })

	// Pull each neighbour's sources in trust order: a source already
	// among the seeds has nothing new to add, and a source reachable
	// through more than one neighbour is only fetched once, at the
	// rank of the first (highest-trust) neighbour that reaches it.
	var out []anamnesia.SearchHit
	fetched := map[uuid.UUID]bool{}
	for _, n := range ranked {
		if len(out) >= q.GraphK {
			break
		}
		srcIDs, err := e.Store.SourcesForEntities(ctx, []uuid.UUID{n.id})
		if err != nil {
			return nil, fmt.Errorf("sources for entities: %w", err)
		}
		var fresh []uuid.UUID
		for _, sid := range srcIDs {
			if seedSources[sid] || fetched[sid] {
				continue
			}
			fetched[sid] = true
			fresh = append(fresh, sid)
		}
		if len(fresh) == 0 {
			continue
		}
		hits, err := e.hitsForSources(ctx, q.Scope, fresh, wantFacts, wantExperiences, q.OnlyRaw)
		if err != nil {
			return nil, err
		}
		out = append(out, hits...)
	}
	if len(out) > q.GraphK {
		out = out[:q.GraphK]
	}
	return out, nil
}

// hitsForSources loads the facts and/or experiences a set of sources
// produced, under the same scope and OnlyRaw restriction the rest of
// retrieval enforces.
func (e *Engine) hitsForSources(ctx context.Context, scope anamnesia.Scope, sourceIDs []uuid.UUID, wantFacts, wantExperiences, onlyRaw bool) ([]anamnesia.SearchHit, error) {
	var out []anamnesia.SearchHit
	if wantFacts {
		args := []any{sourceIDs, scope.UserID}
		where := []string{"source_id = ANY($1)", "user_id = $2", "deleted_at IS NULL"}
		if scope.ProjectID != nil {
			args = append(args, *scope.ProjectID)
			where = append(where, fmt.Sprintf("(project_id = $%d OR project_id IS NULL)", len(args)))
		}
		q := fmt.Sprintf(`
			SELECT id, user_id, project_id, source_id, fact_scope, key, value, source, trust, pii_tags,
			       embed_model, valid_from, valid_to, ingested_at, invalidated_at,
			       superseded_by, deleted_at
			FROM facts WHERE %s`, strings.Join(where, " AND "))
		facts, err := e.Store.QueryFacts(ctx, q, args)
		if err != nil {
			return nil, fmt.Errorf("facts for sources: %w", err)
		}
		for _, f := range facts {
			out = append(out, anamnesia.SearchHit{Domain: anamnesia.DomainFact, Fact: f})
		}
	}
	if wantExperiences {
		args := []any{sourceIDs, scope.UserID}
		where := []string{"source_id = ANY($1)", "user_id = $2", "deleted_at IS NULL", "invalidated_at IS NULL"}
		if scope.ProjectID != nil {
			args = append(args, *scope.ProjectID)
			where = append(where, fmt.Sprintf("(project_id = $%d OR project_id IS NULL)", len(args)))
		}
		if onlyRaw {
			where = append(where, "abstraction = 0")
		}
		q := fmt.Sprintf(`SELECT id, user_id, project_id, source_id, kind, abstraction, title, body, outcome, meta,
			trust, importance, relevance, pii_tags, use_count, last_used_at, embed_model,
			valid_from, valid_to, ingested_at, invalidated_at, superseded_by, deleted_at,
			occurred_at, participants, topic, parent_id, provenance
			FROM experiences WHERE %s`, strings.Join(where, " AND "))
		exps, err := e.Store.QueryExperiences(ctx, q, args)
		if err != nil {
			return nil, fmt.Errorf("experiences for sources: %w", err)
		}
		for _, x := range exps {
			out = append(out, anamnesia.SearchHit{Domain: anamnesia.DomainExperience, Experience: x})
		}
	}
	return out, nil
}

// hitSourceID reads the source a hit came from, when it has one. Skills
// don't carry a source_id, so a skill hit never seeds the graph walk.
func hitSourceID(h anamnesia.SearchHit) *uuid.UUID {
	switch h.Domain {
	case anamnesia.DomainFact:
		if h.Fact != nil {
			return h.Fact.SourceID
		}
	case anamnesia.DomainExperience:
		if h.Experience != nil {
			return h.Experience.SourceID
		}
	}
	return nil
}
