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

// maxGraphSeedEntities bounds how many entities a batch of seed sources
// can hand the walk. EntitiesForSources is uncapped: a hub entity ("the
// user", a repo name) mentioned by many sources would otherwise make the
// number of Neighbors calls below unbounded, on a hot path with a 2.5s
// budget.
const maxGraphSeedEntities = 20

// graphExpand walks out from the top q.GraphSeedN hits of fused (the
// vector+lexical fused ranking, already sorted) and returns up to
// q.GraphK extra candidate hits reachable by one edge hop:
//
//  1. collect the seed hits' source_ids
//  2. EntitiesForSources → the entities those sources mention
//  3. Neighbors(entity, nil, "both", GraphFanout) for each, restricted
//     to edges valid now
//  4. SourcesForEntities on the neighbours, batched into one call → their
//     sources
//  5. facts and experiences from those sources, ranked by each row's own
//     trust
//
// Cheap no-op whenever the graph has nothing to say about these hits —
// which is every install today: step 2 finds no entities, the walk over
// them makes zero further Neighbors calls, and this returns with nothing
// to show for it.
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
	// Scope predicate #1: EntitiesForSources and Neighbors (Task 1-3)
	// take no scope of their own, so this is the walk's own boundary —
	// independent of the user_id filter in hitsForSources below. A
	// mention recorded against the wrong scope, or a cross-tenant edge
	// (nothing stops one being created; see anamnesia_graph_edge), must
	// not be able to seed the walk in the first place.
	entities = entitiesInScope(entities, q.Scope)
	if len(entities) > maxGraphSeedEntities {
		entities = entities[:maxGraphSeedEntities]
	}
	if len(entities) == 0 {
		return nil, nil
	}

	// Walk out from every seed entity, both directions, keeping the
	// highest-trust edge that reaches each neighbour. One Neighbors call
	// per seed entity — that count is what maxGraphSeedEntities bounds.
	bestTrust := map[uuid.UUID]float32{}
	for _, ent := range entities {
		neighbors, edges, err := e.Store.Neighbors(ctx, ent.ID, nil, "both", q.GraphFanout)
		if err != nil {
			return nil, fmt.Errorf("neighbors: %w", err)
		}
		for i, n := range neighbors {
			// Scope predicate #2: same reasoning as above, applied to
			// the far end of the edge.
			if !inScope(n.Scope, q.Scope) {
				continue
			}
			if t := edges[i].Trust; t > bestTrust[n.ID] {
				bestTrust[n.ID] = t
			}
		}
	}
	if len(bestTrust) == 0 {
		return nil, nil
	}
	neighborIDs := make([]uuid.UUID, 0, len(bestTrust))
	for id := range bestTrust {
		neighborIDs = append(neighborIDs, id)
	}

	// One batched call for every neighbour's sources, not one call per
	// neighbour: SourcesForEntities doesn't report which neighbour
	// reached which source, so exact per-source edge-trust order isn't
	// recoverable from a single call — the trade this makes for O(1)
	// round trips instead of O(|neighbours|). The candidates it returns
	// are ranked by their own row-level trust instead, below.
	srcIDs, err := e.Store.SourcesForEntities(ctx, neighborIDs)
	if err != nil {
		return nil, fmt.Errorf("sources for entities: %w", err)
	}
	var fresh []uuid.UUID
	for _, sid := range srcIDs {
		if !seedSources[sid] {
			fresh = append(fresh, sid)
		}
	}
	if len(fresh) == 0 {
		return nil, nil
	}

	out, err := e.hitsForSources(ctx, q.Scope, fresh, wantFacts, wantExperiences, q.OnlyRaw, q.GraphK)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return hitTrust(out[i]) > hitTrust(out[j]) })
	if len(out) > q.GraphK {
		out = out[:q.GraphK]
	}
	return out, nil
}

// hitsForSources loads the facts and/or experiences a set of sources
// produced, under the same scope and OnlyRaw restriction the rest of
// retrieval enforces, and no more than `limit` of each: without a LIMIT
// here, a hub entity mentioned by many sources would pull every fact and
// experience those sources ever produced into memory before graphExpand's
// own GraphK truncation ever runs.
func (e *Engine) hitsForSources(ctx context.Context, scope anamnesia.Scope, sourceIDs []uuid.UUID, wantFacts, wantExperiences, onlyRaw bool, limit int) ([]anamnesia.SearchHit, error) {
	var out []anamnesia.SearchHit
	if wantFacts {
		args := []any{sourceIDs, scope.UserID}
		where := []string{"source_id = ANY($1)", "user_id = $2", "deleted_at IS NULL"}
		if scope.ProjectID != nil {
			args = append(args, *scope.ProjectID)
			where = append(where, fmt.Sprintf("(project_id = $%d OR project_id IS NULL)", len(args)))
		}
		args = append(args, limit)
		q := fmt.Sprintf(`
			SELECT id, user_id, project_id, source_id, fact_scope, key, value, source, trust, pii_tags,
			       embed_model, valid_from, valid_to, ingested_at, invalidated_at,
			       superseded_by, deleted_at
			FROM facts WHERE %s
			ORDER BY trust DESC
			LIMIT $%d`, strings.Join(where, " AND "), len(args))
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
		args = append(args, limit)
		q := fmt.Sprintf(`SELECT id, user_id, project_id, source_id, kind, abstraction, title, body, outcome, meta,
			trust, importance, relevance, pii_tags, use_count, last_used_at, embed_model,
			valid_from, valid_to, ingested_at, invalidated_at, superseded_by, deleted_at,
			occurred_at, participants, topic, parent_id, provenance
			FROM experiences WHERE %s
			ORDER BY trust DESC
			LIMIT $%d`, strings.Join(where, " AND "), len(args))
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

// hitTrust reads a hit's own row-level trust, used to rank graph
// candidates once batching (see graphExpand) gives up exact edge-trust
// order.
func hitTrust(h anamnesia.SearchHit) float32 {
	switch h.Domain {
	case anamnesia.DomainFact:
		if h.Fact != nil {
			return h.Fact.Trust
		}
	case anamnesia.DomainExperience:
		if h.Experience != nil {
			return h.Experience.Trust
		}
	}
	return 0
}

// inScope reports whether candidate belongs to want's tenant boundary,
// mirroring the "(project_id = $x OR project_id IS NULL)" rule the
// vector/lexical channels already apply in SQL: that clause is only ever
// added when the query itself is project-scoped (want.ProjectID set), in
// which case a user-level row (candidate.ProjectID nil) is visible
// alongside an exact project match — a user-level query (want.ProjectID
// nil) carries no project restriction at all, and sees every project.
func inScope(candidate, want anamnesia.Scope) bool {
	if candidate.UserID != want.UserID {
		return false
	}
	if want.ProjectID == nil {
		return true
	}
	return candidate.ProjectID == nil || *candidate.ProjectID == *want.ProjectID
}

// entitiesInScope filters a slice of entities down to those in scope, in
// place.
func entitiesInScope(ents []*anamnesia.Entity, scope anamnesia.Scope) []*anamnesia.Entity {
	out := ents[:0]
	for _, ent := range ents {
		if inScope(ent.Scope, scope) {
			out = append(out, ent)
		}
	}
	return out
}
