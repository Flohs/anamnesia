// The graph pass: a second, separate extraction that reads the WHOLE
// checkpoint's text once (not per segment — see the "Revised 2026-08-19"
// note in docs/superpowers/specs/2026-08-18-graph-extraction-design.md)
// and asks only for ADD_ENTITY / ADD_EDGE / NOOP. It never runs the
// fact/experience pass, the surprise gate, or the candidate fetch: it is
// a different job on the same queue, reached via the graphSourceKind
// branch in Run.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/embed"
	"github.com/flohs/anamnesia/internal/llm"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// graphSourceKind marks a source as carrying a whole checkpoint's text
// for the graph pass, posted once per checkpoint alongside the normal
// per-segment sources.
const graphSourceKind = "claude-session-graph"

// graphOperation is one ADD_ENTITY / ADD_EDGE / NOOP the model emitted.
// Edges name entities by name, not id — the model has never seen a uuid.
type graphOperation struct {
	Op    string         `json:"op"` // ADD_ENTITY | ADD_EDGE | NOOP
	Kind  string         `json:"kind,omitempty"`
	Name  string         `json:"name,omitempty"`
	From  string         `json:"from,omitempty"`
	To    string         `json:"to,omitempty"`
	Props map[string]any `json:"props,omitempty"`
	Trust float32        `json:"trust,omitempty"`
}

// graphOperationSchema is the JSON Schema for the graph pass's response,
// the same envelope shape as operationSchema (extract.go:648) but with a
// disjoint set of operations — this schema has no ADD_FACT/ADD_EXPERIENCE
// and operationSchema has no ADD_ENTITY/ADD_EDGE, so a malformed response
// from one pass can never be mistaken for the other's shape.
var graphOperationSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "operations": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "op":    {"type": "string", "enum": ["ADD_ENTITY","ADD_EDGE","NOOP"]},
          "kind":  {"type": "string"},
          "name":  {"type": "string"},
          "from":  {"type": "string"},
          "to":    {"type": "string"},
          "props": {},
          "trust": {"type": "number"}
        },
        "required": ["op"]
      }
    }
  },
  "required": ["operations"]
}`)

const graphSystemPrompt = `You are the graph extraction pass for an agent memory system. You read the FULL TEXT of one checkpoint — not a single topic segment — and decide what durable entities and relationships it describes.

You can emit these operations:
- ADD_ENTITY: a durable, nameable thing worth its own node — a person, place, project, service, organisation, or system. Provide "kind" (a short noun: person|place|project|service|organisation|system|...) and "name" in lower case. Optional "props" for attributes worth keeping (e.g. {"role": "nightly job"}).
- ADD_EDGE: a durable relationship between two entities. Provide "from" and "to" (entity names, lower case, exactly matching an ADD_ENTITY name), "kind" (a short verb phrase: reads_from|reports_to|owns|prefers|...), and optional "trust" in [0,1].
- NOOP: this checkpoint describes no relationships worth keeping as graph edges.

Rules:
- Name entities in lower case, and use the same name every time the same thing is meant, or edges will not resolve.
- Prefer FEW durable relationships over many incidental ones. A relationship earns an edge when it would still be true next month; a one-off action does not.
- Default to NOOP when nothing durable is described. Most checkpoints describe activity, not structure.
- Output JSON only, matching: {"operations":[{"op":"...", ...}]}. No prose, no markdown fences.
- Keep trust in [0,1]. Default 0.7 if you have no other signal.

The payload's "candidates" list, when present, holds entities that may already exist in this scope: each has a "body" (its name) and "meta.kind" (its kind). They are a recall net, not a verdict — some may be irrelevant. Before an ADD_ENTITY, check whether a candidate is the SAME real-world thing this text means, even under a different spelling ("Priha Raman" vs "priya-raman" is the same person misspelled) — if so, use ITS EXACT name and kind, so the two land on one node instead of forking. A candidate only matches if the kind agrees too: the same name under a different kind is a different thing (a service and a project sharing a name are not interchangeable), and two similar names can still be two different people, versions, or opposites (a read replica is not a write replica). When unsure, or when nothing fits, create a new entity — a fork is easy to fix later, a wrong merge is not.`

// normaliseEntityName canonicalises a name so upsert dedupes on meaning
// rather than on the literal string ON CONFLICT sees: "The Rotterdam
// Warehouse", "Rotterdam warehouse" and "rotterdam  warehouse" must all
// become the same node. slugKey (extract.go:514) already lower-cases,
// collapses runs of non-alphanumerics to one hyphen, and trims — this
// only adds stripping a leading "the", which slugKey has no reason to
// know about.
func normaliseEntityName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.TrimPrefix(s, "the ")
	return slugKey(s)
}

// graphCandidateK is how many nearest entities to offer the model as
// candidates. Generous on purpose: this is recall, not a decision, so
// the cost of including one too many is a slightly longer prompt, not a
// wrong merge — see entityCandidatesWith.
const graphCandidateK = 10

// entityCandidates embeds the checkpoint's content and asks the store
// for the entities in scope whose name embedding is nearest to it,
// keeping only those within graph.candidate_distance. It does NOT
// decide identity — see entityCandidatesWith for why a distance number
// cannot — it only recalls entities worth putting in front of the model
// before the (single) graph model call, so a name mentioned under a new
// spelling can be recognised as one already known.
func (e *Extractor) entityCandidates(ctx context.Context, scope anamnesia.Scope, content string) []store.EntityMatch {
	threshold := e.Cfg.applyDefaults().GraphCandidateDistance
	return entityCandidatesWith(ctx, e.Embedder, e.Store.NearestEntities, threshold, scope, content, graphCandidateK, e.Log)
}

// entityCandidatesWith is entityCandidates' logic with the store call
// taken as a function value instead of a *store.Store, so recall can be
// unit tested without a database.
//
// Distance-threshold MERGING was tried and rejected: measured against
// the real embedder, priya-raman/priha-raman (0.2349, the motivating
// typo, must merge) sits CLOSER than priya-raman/priya-ramanujan
// (0.1165, a different person, must not), auth-service/auth-service-v2
// (0.1203) and read-replica/write-replica (0.1365, opposites) — no
// threshold separates the must-merge case from the must-not-merge
// cases, because short names embed by shared prefix/token overlap, not
// by meaning. So this function only narrows a candidate list; the
// graph system prompt asks the model to judge identity — it knows
// Ramanujan isn't Raman and a read replica isn't a write replica, which
// a distance cannot.
//
// The same-kind guard now lives in two places, both load-bearing:
// each candidate carries its kind into the prompt (graphSystemPrompt
// tells the model a shared name under a different kind is a different
// thing — "rotterdam" the place vs "rotterdam-warehouse" the site
// embed within a plausible distance, per Ruling 5 in
// .superpowers/sdd/2026-08-20-entity-resolution/progress.md), and the
// entities table's unique index is on (scope, kind, name) — so even if
// the model reuses a candidate's name under the WRONG kind, ON CONFLICT
// does not fire and a new row is created rather than a wrong merge.
// A fork is recoverable; a wrong merge is not, so the schema itself
// makes the irreversible failure structurally impossible.
//
// An embedder or lookup failure must not fail the graph pass (a graph
// that stops extracting is worse than one that occasionally forks), so
// either simply yields no candidates: the model still extracts, using
// only its own judgment of the text, same as before this recall step
// existed.
func entityCandidatesWith(
	ctx context.Context,
	embedder embed.Embedder,
	nearest func(ctx context.Context, scope anamnesia.Scope, vec []float32, limit int) ([]store.EntityMatch, error),
	threshold float64,
	scope anamnesia.Scope, content string, k int,
	log *slog.Logger,
) []store.EntityMatch {
	if embedder == nil {
		return nil
	}
	vecs, err := embedder.Embed(ctx, []string{content})
	if err != nil {
		if log != nil {
			log.Warn("extractor: embed checkpoint content for entity candidates failed", "err", err)
		}
		return nil
	}
	if len(vecs) == 0 {
		return nil
	}
	matches, err := nearest(ctx, scope, vecs[0], k)
	if err != nil {
		if log != nil {
			log.Warn("extractor: nearest-entity candidate lookup failed", "err", err)
		}
		return nil
	}
	out := make([]store.EntityMatch, 0, len(matches))
	for _, m := range matches {
		if m.Distance <= threshold {
			out = append(out, m)
		}
	}
	return out
}

// entityCandidateList turns nearest-entity matches into the candidate
// shape userPrompt already knows how to send (see extract.go): domain
// "entity", the entity's id, its name as Body, and its kind in Meta —
// exactly what graphSystemPrompt tells the model to read.
func entityCandidateList(matches []store.EntityMatch) []candidate {
	out := make([]candidate, 0, len(matches))
	for _, m := range matches {
		out = append(out, candidate{
			Domain: "entity",
			ID:     m.Entity.ID.String(),
			Body:   m.Entity.Name,
			Meta:   map[string]any{"kind": m.Entity.Kind},
		})
	}
	return out
}

// embedEntityName is a best-effort embed of one entity's name, so a
// newly created entity carries a vector for a later checkpoint's
// entityCandidates call to find it by. nil on any failure — an embedder
// problem must not fail the graph pass, and UpsertEntity already treats
// a nil Embedding as "no vector yet" rather than an error.
func (e *Extractor) embedEntityName(ctx context.Context, name string) []float32 {
	if e.Embedder == nil {
		return nil
	}
	vecs, err := e.Embedder.Embed(ctx, []string{name})
	if err != nil {
		if e.Log != nil {
			e.Log.Warn("extractor: embed entity name failed", "name", name, "err", err)
		}
		return nil
	}
	if len(vecs) == 0 {
		return nil
	}
	return vecs[0]
}

// resolveEdges turns ADD_EDGE operations naming entities by name into
// Edge rows naming them by id. known maps a normalised entity name to
// the id UpsertEntity assigned it — either freshly, this pass, or looked
// up from an earlier checkpoint. An edge whose endpoint does not resolve
// is dropped rather than silently discarded, with a reason recorded so
// the trace can say why the graph looks the way it does.
func resolveEdges(ops []graphOperation, known map[string]uuid.UUID) (resolved []anamnesia.Edge, dropped []string) {
	for _, op := range ops {
		if strings.ToUpper(strings.TrimSpace(op.Op)) != "ADD_EDGE" {
			continue
		}
		fromID, fromOK := known[normaliseEntityName(op.From)]
		toID, toOK := known[normaliseEntityName(op.To)]
		switch {
		case !fromOK && !toOK:
			dropped = append(dropped, fmt.Sprintf("edge %q -> %q (%s): neither endpoint resolved to an entity", op.From, op.To, op.Kind))
		case !fromOK:
			dropped = append(dropped, fmt.Sprintf("edge %q -> %q (%s): %q did not resolve to an entity", op.From, op.To, op.Kind, op.From))
		case !toOK:
			dropped = append(dropped, fmt.Sprintf("edge %q -> %q (%s): %q did not resolve to an entity", op.From, op.To, op.Kind, op.To))
		default:
			resolved = append(resolved, anamnesia.Edge{From: fromID, To: toID, Kind: op.Kind, Props: op.Props, Trust: op.Trust})
		}
	}
	return resolved, dropped
}

// runGraph is the graph pass for one claude-session-graph source: one
// model call, then entity upserts, edge resolution and edge writes
// against the store.
func (e *Extractor) runGraph(ctx context.Context, src *anamnesia.Source, tr *activity.Trace) (int, error) {
	cfg := e.Cfg.applyDefaults()
	content := strings.TrimSpace(src.RawContent)

	// Candidate recall happens before the (single) model call, not after
	// it: it exists to help the model name an entity consistently with
	// what already exists, not to second-guess what it named. See
	// entityCandidatesWith for why this is recall, not a merge decision.
	candMatches := e.entityCandidates(ctx, src.Scope, content)
	cands := entityCandidateList(candMatches)
	prompt := e.userPrompt(src, content, cands)

	captured := &capturedOps{}
	started := time.Now()
	if err := e.LLM.Extract(ctx, llm.DistillInput{
		System:     graphSystemPrompt,
		User:       prompt,
		MaxTok:     1024,
		Schema:     graphOperationSchema,
		SchemaName: "anamnesia_graph_operations",
	}, captured); err != nil {
		tr.Fail("llm", err)
		tr.End("failed", "The model call failed, so the source stays pending")
		return 0, fmt.Errorf("llm extract: %w", err)
	}
	resp := captured.resp
	tr.Step("llm", fmt.Sprintf("%s returned %d graph operations", e.LLM.Model(), len(resp.Operations)),
		map[string]any{
			"model":                     e.LLM.Model(),
			"latency_ms":                time.Since(started).Milliseconds(),
			"prompt_chars":              len(graphSystemPrompt) + len(prompt),
			"completion_chars":          len(captured.raw),
			"raw_response":              string(captured.raw),
			"entity_candidates_offered": cands,
		})

	if len(resp.Operations) > cfg.GraphMaxOps {
		resp.Operations = resp.Operations[:cfg.GraphMaxOps]
	}

	ops := make([]graphOperation, 0, len(resp.Operations))
	for i, raw := range resp.Operations {
		var op graphOperation
		if err := json.Unmarshal(raw, &op); err != nil {
			if e.Log != nil {
				e.Log.Warn("extractor: graph op decode failed", "idx", i, "err", err, "raw", string(raw))
			}
			continue
		}
		if isNoop(op.Op) {
			continue
		}
		ops = append(ops, op)
	}

	// Upsert every ADD_ENTITY op, normalised, collecting ids by name.
	// Identity itself was decided upstream of this: the candidates
	// offered above give the model the chance to reuse an existing
	// name/kind for the same real-world thing, and the ordinary
	// (scope, kind, name) upsert then lands both on one row. This loop
	// does not judge — a name the model chose not to reuse creates a
	// new entity, same as it always has.
	known := make(map[string]uuid.UUID)
	upserted := make([]*anamnesia.Entity, 0, len(ops))
	failures := make([]string, 0)
	for _, op := range ops {
		if strings.ToUpper(strings.TrimSpace(op.Op)) != "ADD_ENTITY" {
			continue
		}
		name := normaliseEntityName(op.Name)
		if name == "" || op.Kind == "" {
			failures = append(failures, fmt.Sprintf("ADD_ENTITY %q: kind and name required", op.Name))
			continue
		}
		ent := &anamnesia.Entity{Scope: src.Scope, Kind: op.Kind, Name: name, Props: op.Props}
		// Attach the name's embedding (best-effort; nil on any failure —
		// UpsertEntity already treats a nil Embedding as "no vector yet")
		// so THIS entity becomes findable as a candidate the next time
		// something nearby is checkpointed. Per Ruling 3
		// (.superpowers/sdd/2026-08-20-entity-resolution/progress.md),
		// only new entities get this — no backfill of entities created
		// before candidate recall existed.
		ent.Embedding = e.embedEntityName(ctx, name)
		if err := e.Store.UpsertEntity(ctx, ent); err != nil {
			if e.Log != nil {
				e.Log.Warn("extractor: upsert entity failed", "err", err)
			}
			failures = append(failures, "ADD_ENTITY "+name+": "+err.Error())
			continue
		}
		known[name] = ent.ID
		upserted = append(upserted, ent)
	}

	// Resolve edge endpoints not created this pass against entities from
	// an earlier checkpoint, by name alone — a bare edge endpoint carries
	// no kind, so LookupEntity (which needs one) cannot be used here.
	// Resolve only when the name is unambiguous: two entities sharing a
	// name under different kinds means the edge is dropped, not guessed.
	// A wrong edge would be believed; a missing one is merely invisible.
	var ambiguous []string
	queried := make(map[string]bool)
	for _, op := range ops {
		if strings.ToUpper(strings.TrimSpace(op.Op)) != "ADD_EDGE" {
			continue
		}
		for _, raw := range []string{op.From, op.To} {
			name := normaliseEntityName(raw)
			if name == "" || queried[name] {
				continue
			}
			if _, ok := known[name]; ok {
				continue
			}
			queried[name] = true
			matches, err := e.Store.LookupEntitiesByName(ctx, src.Scope, name)
			if err != nil {
				if e.Log != nil {
					e.Log.Warn("extractor: lookup entity by name failed", "err", err)
				}
				continue
			}
			switch len(matches) {
			case 0:
				// Left unresolved: resolveEdges reports it below.
			case 1:
				known[name] = matches[0].ID
			default:
				ambiguous = append(ambiguous, fmt.Sprintf("entity %q is ambiguous: %d entities share that name across kinds", name, len(matches)))
			}
		}
	}

	resolved, dropped := resolveEdges(ops, known)
	dropped = append(dropped, ambiguous...)

	edgesCreated, edgesSuperseded := 0, 0
	for _, edge := range resolved {
		if existing, err := e.findValidEdge(ctx, edge.From, edge.To, edge.Kind); err == nil && existing != nil {
			if err := e.Store.InvalidateEdge(ctx, existing.ID); err == nil {
				edgesSuperseded++
			} else if e.Log != nil {
				e.Log.Warn("extractor: invalidate edge failed", "err", err)
			}
		}
		ne := edge
		if err := e.Store.CreateEdge(ctx, &ne); err != nil {
			if e.Log != nil {
				e.Log.Warn("extractor: create edge failed", "err", err)
			}
			failures = append(failures, fmt.Sprintf("ADD_EDGE %s->%s: %s", edge.From, edge.To, err.Error()))
			continue
		}
		edgesCreated++
	}

	// Mentions must be recorded against the checkpoint's segment sources,
	// not only the graph source itself: those are the sources a search
	// hit actually carries, and EntitiesForSources joins on source_id
	// exactly (store/graph.go). The graph source keeps a mention too, for
	// provenance, but it must not be the only one — see
	// docs/superpowers/specs/2026-08-19-the-graph-bridge-is-broken.md.
	mentionSources := append([]uuid.UUID{src.ID}, e.segmentSourceIDsFromMetadata(src.Metadata)...)
	for _, id := range known {
		for _, sourceID := range mentionSources {
			if err := e.Store.RecordMention(ctx, id, sourceID); err != nil && e.Log != nil {
				e.Log.Warn("extractor: record mention failed", "err", err)
			}
		}
	}

	tr.Step("graph", fmt.Sprintf("Upserted %d entities, created %d edges (%d superseded, %d dropped)",
		len(upserted), edgesCreated, edgesSuperseded, len(dropped)),
		map[string]any{
			"entities_upserted": len(upserted),
			"edges_created":     edgesCreated,
			"edges_superseded":  edgesSuperseded,
			"edges_dropped":     dropped,
			"failures":          failures,
		})

	executed := len(upserted) + edgesCreated
	if executed == 0 {
		tr.End("skipped", "The model found no durable entities or relationships")
	} else {
		tr.End("ok", fmt.Sprintf("Extracted %d entities and %d edges from a %s %s source",
			len(upserted), edgesCreated, humanBytes(len(content)), src.Kind))
	}
	return executed, nil
}

// segmentSourceIDsFromMetadata reads the checkpoint's segment source ids
// off the graph source's metadata (set by doCheckpoint in cmd/anamnesia/
// hook.go). Tolerant by design: an older checkpoint carries no such key,
// and a graph source posted by something else may not shape it the way
// this expects — either way runGraph must finish with whatever it can
// resolve, not fail the pass over it.
func (e *Extractor) segmentSourceIDsFromMetadata(meta map[string]any) []uuid.UUID {
	raw, ok := meta["segment_source_ids"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		if e.Log != nil {
			e.Log.Warn("extractor: segment_source_ids metadata has an unexpected shape", "type", fmt.Sprintf("%T", raw))
		}
		return nil
	}
	ids := make([]uuid.UUID, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			if e.Log != nil {
				e.Log.Warn("extractor: segment_source_ids entry is not a string", "type", fmt.Sprintf("%T", v))
			}
			continue
		}
		id, err := uuid.Parse(s)
		if err != nil {
			if e.Log != nil {
				e.Log.Warn("extractor: segment_source_ids entry is not a uuid", "value", s, "err", err)
			}
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// findValidEdge looks for a currently-valid edge with the same
// (from, to, kind), so runGraph can supersede it via InvalidateEdge
// instead of creating a duplicate. Neighbors already filters to edges
// valid now (invalidated_at IS NULL AND valid_to IS NULL OR > now()).
func (e *Extractor) findValidEdge(ctx context.Context, from, to uuid.UUID, kind string) (*anamnesia.Edge, error) {
	_, edges, err := e.Store.Neighbors(ctx, from, []string{kind}, "out", 50)
	if err != nil {
		return nil, err
	}
	for _, edge := range edges {
		if edge.To == to {
			return edge, nil
		}
	}
	return nil, nil
}
