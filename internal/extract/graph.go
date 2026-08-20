// The graph pass: a second, separate extraction that reads the WHOLE
// checkpoint's text once (not per segment — see the "Revised 2026-08-19"
// note in docs/superpowers/specs/2026-08-18-graph-extraction-design.md)
// and asks only for ADD_ENTITY / ADD_EDGE / NOOP. It never runs the
// fact/experience pass, the surprise gate, or the candidate fetch: it is
// a different job on the same queue, reached via the graphSourceKind
// branch in Run.
package extract

import (
	"regexp"
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
- Keep trust in [0,1]. Default 0.7 if you have no other signal.`

// graphIdentitySystemPrompt drives the second, conditional model call:
// see resolveIdentities. It never runs unless at least one entity this
// checkpoint just extracted has a nearby existing entity to weigh
// against, so most checkpoints never pay for it.
const graphIdentitySystemPrompt = `You are resolving entity identity for a memory graph. You will be given entities just extracted from a checkpoint, each paired with one or more CANDIDATES: existing entities in the same scope whose name embedded close to it. A candidate is a possible match, not a confirmed one.

For each entity, decide: is it the SAME real-world thing as one of its candidates, merely named or spelled differently ("Priha Raman" vs "priya-raman" is the same person misspelled) — or a DIFFERENT thing that happens to have a similar name (a different person, a different version, an opposite)? "priya-ramanujan" is a different person from "priya-raman", not a longer spelling of the same one. A read replica is not a write replica. A service and a project sharing a name are not interchangeable.

Only report "same_as" when you are genuinely confident, and only with a candidate id offered for THAT entity. When unsure, or when none of its candidates fit, omit "same_as" — a duplicate entity is easy to fix later; a wrongly merged one is not.

Echo back the "kind" each entity was given along with its name. Two entities in one checkpoint can share a name under different kinds, and the kind is what says which of them your verdict is about.

Output JSON only, matching: {"verdicts":[{"entity":"...","kind":"...","same_as":"<a candidate id from that entity's own list, or omit if none match>"}]}. No prose, no markdown fences.`

// identityVerdictSchema is the JSON Schema for the identity
// disambiguation call's response — a disjoint shape from
// graphOperationSchema, same reasoning as that schema's own comment.
var identityVerdictSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "verdicts": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "entity":  {"type": "string"},
          "kind":    {"type": "string"},
          "same_as": {"type": "string"}
        },
        "required": ["entity","kind"]
      }
    }
  },
  "required": ["verdicts"]
}`)

// identityVerdict is one "same or different" judgment from the identity
// disambiguation call. SameAs is a candidate's id, or empty for "this is
// a different, new entity".
//
// Kind is what makes a verdict attributable to exactly one of the
// entities we asked about: one checkpoint can extract two entities
// sharing a name under different kinds, and each is asked about
// separately (they get different candidates, because recall filters by
// kind). Without the kind echoed back, both would consume the same
// verdict — see resolveIdentities' verdict loop.
type identityVerdict struct {
	Entity string `json:"entity"`
	Kind   string `json:"kind,omitempty"`
	SameAs string `json:"same_as,omitempty"`
}

type identityVerdictResponse struct {
	Verdicts []identityVerdict `json:"verdicts"`
}

// capturedVerdicts decodes the verdicts envelope while keeping the raw
// JSON for the trace, the same technique capturedOps uses (extract.go)
// and for the same reason: every provider hands `out` to json.Unmarshal,
// so implementing Unmarshaler is enough to see the document without
// touching the llm package.
type capturedVerdicts struct {
	raw  json.RawMessage
	resp identityVerdictResponse
}

func (c *capturedVerdicts) UnmarshalJSON(b []byte) error {
	c.raw = append(c.raw[:0], b...)
	return json.Unmarshal(b, &c.resp)
}

// identityCandidateJSON and identityQuery shape the identity
// disambiguation call's payload: one entry per extracted entity that
// has at least one candidate, each candidate carrying only what the
// model needs — an id to answer with, a name and kind to judge by.
type identityCandidateJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}
type identityQuery struct {
	Entity     string                  `json:"entity"`
	Kind       string                  `json:"kind"`
	Candidates []identityCandidateJSON `json:"candidates"`
}

// NormaliseEntityName canonicalises a name so upsert dedupes on meaning
// rather than on the literal string ON CONFLICT sees: "The Rotterdam
// Warehouse", "Rotterdam warehouse" and "rotterdam  warehouse" must all
// become the same node. slugKey (extract.go:514) already lower-cases,
// collapses runs of non-alphanumerics to one hyphen, and trims — this
// only adds stripping a leading "the", which slugKey has no reason to
// know about.
func NormaliseEntityName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.TrimPrefix(s, "the ")
	return slugKey(s)
}

// NormaliseEntityKind canonicalises a kind for exactly the same reason
// NormaliseEntityName canonicalises a name, and it matters just as much:
// kind is half of the (scope, kind, name) unique key AND the whole of
// the same-kind candidate filter, yet nothing constrains what ends up in
// it — graphOperationSchema types it as a bare string with no enum, the
// prompt asks for lower-case names and says nothing about kind, and
// anamnesia_graph_entity upserts whatever the caller passes. A model
// writing "Person" in one checkpoint and "person" in the next would
// otherwise produce two entities that can never be offered as each
// other's candidate: entity resolution silently inert for that pair,
// forever, with no log line. Unlike a name, a kind has no leading "the"
// to strip, so this is slugKey alone.
func NormaliseEntityKind(kind string) string {
	return slugKey(kind)
}

// normaliseEdgeKind canonicalises an edge's kind. Nothing constrains what
// the model writes there, so one relation arrives as "depends on",
// "depends_on" and "Depends On" across three checkpoints and the graph
// describes it three ways.
//
// Not slugKey, deliberately: slugKey rewrites "_" to "-", and every edge
// kind this codebase already writes is underscored — reads_from,
// depends_on, reports_to, escalation_contact. Running names through
// slugKey would rename the entire existing corpus of edge kinds to a new
// spelling and fork them against every row written before it. Lowercase,
// trim, and collapse any run of separators to a single underscore: three
// spellings converge, and a kind already in the house style is returned
// exactly as it came in.
func normaliseEdgeKind(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	k = edgeKindSepRE.ReplaceAllString(k, "_")
	return strings.Trim(k, "_")
}

var edgeKindSepRE = regexp.MustCompile(`[^a-z0-9]+`)

// entityKey is the identity an extracted entity is tracked under while a
// checkpoint executes: kind AND name, never name alone. (scope, kind,
// name) is what the unique index calls one entity, and the same-kind
// candidate filter is only load-bearing if every map downstream of it
// keeps the kind too — keyed by bare name, a verdict about a `project`
// gets applied to a `place` of the same name, and a city is merged into
// a warehouse. Both halves are already normalised by the time they get
// here; NUL separates them because slugKey can never produce one, so no
// two distinct (kind, name) pairs can collide on a key.
func entityKey(kind, name string) string {
	return kind + "\x00" + name
}

// graphIdentityCandidateK is how many nearest same-named entities to
// consider per extracted entity before asking the model to judge them.
// Small on purpose: unlike a broad content-based recall, this is a
// specific name lookup, so a handful of nearest neighbours is enough
// for the model to see every plausible match.
const graphIdentityCandidateK = 3

// entityCandidatesForName embeds one extracted entity's name and asks
// the store for existing entities of the SAME kind within
// graph.candidate_distance. This is recall, not a decision — identity
// is judged by resolveIdentities' model call, never by this distance.
//
// Distance-threshold MERGING (deciding identity from this distance
// alone) was tried and rejected: measured against the real embedder,
// priya-raman/priha-raman (0.2349, the motivating typo, must merge)
// sits CLOSER than priya-raman/priya-ramanujan (0.1165, a different
// person, must not), auth-service/auth-service-v2 (0.1203) and
// read-replica/write-replica (0.1365, opposites) — no threshold
// separates the must-merge case from the must-not-merge cases, because
// short names embed by shared prefix/token overlap, not by meaning.
//
// The same-kind filter is applied HERE, before the model ever sees
// anything — not as a check on its answer. That makes the guard
// load-bearing rather than defensive: measured against the real
// embedder, "rotterdam" (place) and "rotterdam-warehouse" (site) sit at
// 0.2256 — inside any workable candidate bound — so without this filter
// a city would be offered as a candidate for a warehouse inside it
// (Ruling 5, .superpowers/sdd/2026-08-20-entity-resolution/progress.md).
// Filtering here means a cross-kind pair is never even asked about; the
// (scope, kind, name) unique index is a second, independent line of
// defence in case this filter is ever bypassed or removed.
//
// An embedder or lookup failure must not fail the graph pass, so either
// simply yields no candidates for this entity — it still gets created,
// using the model's own judgment of the extraction text, same as before
// candidate recall existed.
func (e *Extractor) entityCandidatesForName(ctx context.Context, scope anamnesia.Scope, kind, name string) []store.EntityMatch {
	threshold := e.Cfg.applyDefaults().GraphCandidateDistance
	return entityCandidatesForNameWith(ctx, e.Embedder, e.Store.NearestEntities, threshold, scope, kind, name, graphIdentityCandidateK, e.Log)
}

// entityCandidatesForNameWith is entityCandidatesForName's logic with
// the store call taken as a function value instead of a *store.Store,
// so recall (including the same-kind filter) can be unit tested
// without a database.
func entityCandidatesForNameWith(
	ctx context.Context,
	embedder embed.Embedder,
	nearest func(ctx context.Context, scope anamnesia.Scope, vec []float32, limit int) ([]store.EntityMatch, error),
	threshold float64,
	scope anamnesia.Scope, kind, name string, k int,
	log *slog.Logger,
) []store.EntityMatch {
	if embedder == nil {
		return nil
	}
	vecs, err := embedder.Embed(ctx, []string{name})
	if err != nil {
		if log != nil {
			log.Warn("extractor: embed entity name for candidate recall failed", "name", name, "err", err)
		}
		return nil
	}
	if len(vecs) == 0 {
		return nil
	}
	matches, err := nearest(ctx, scope, vecs[0], k)
	if err != nil {
		if log != nil {
			log.Warn("extractor: nearest-entity candidate lookup failed", "name", name, "err", err)
		}
		return nil
	}
	out := make([]store.EntityMatch, 0, len(matches))
	want := NormaliseEntityKind(kind)
	for _, m := range matches {
		// Both sides normalised: a stored kind can carry any casing (an
		// entity written before normalisation existed, or one written
		// through anamnesia_graph_entity, which upserts a raw kind), and
		// a candidate must not be hidden by that. See
		// NormaliseEntityKind.
		if m.Distance <= threshold && NormaliseEntityKind(m.Entity.Kind) == want {
			out = append(out, m)
		}
	}
	return out
}

// entityCandidateSet is one ADD_ENTITY op's normalised name/kind
// alongside the existing entities entityCandidatesForName found for
// it — input to the identity disambiguation call, not yet a decision.
type entityCandidateSet struct {
	Name    string
	Kind    string
	Matches []store.EntityMatch
}

// resolveIdentities gathers, for every ADD_ENTITY op, existing entities
// recall found nearby (same kind, within graph.candidate_distance) and
// — only when at least one entity has any — makes ONE additional model
// call for the whole checkpoint, presenting each extracted name against
// its candidates and asking the model to affirm or reject each. Most
// checkpoints have no candidates and pay for no extra call.
//
// The returned map is keyed by entityKey — kind and name, matching what
// runGraph looks an op up by — and holds only pairs the model affirmed
// AND that name a candidate id actually offered for that entity: a
// hallucinated or mismatched id is dropped, not trusted. A call that
// errors returns nil: per instruction, an unavailable judge falls back
// to creating every entity separately, never to merging, because a
// guessed merge is exactly the failure this whole design exists to
// avoid.
func (e *Extractor) resolveIdentities(ctx context.Context, scope anamnesia.Scope, ops []graphOperation, tr *activity.Trace) map[string]uuid.UUID {
	var sets []entityCandidateSet
	for _, op := range ops {
		if strings.ToUpper(strings.TrimSpace(op.Op)) != "ADD_ENTITY" {
			continue
		}
		name := NormaliseEntityName(op.Name)
		kind := NormaliseEntityKind(op.Kind)
		if name == "" || kind == "" {
			continue
		}
		if matches := e.entityCandidatesForName(ctx, scope, kind, name); len(matches) > 0 {
			sets = append(sets, entityCandidateSet{Name: name, Kind: kind, Matches: matches})
		}
	}
	if len(sets) == 0 {
		tr.Step("identity", "No extracted entity had a nearby existing candidate; nothing to disambiguate",
			map[string]any{"entities_with_candidates": 0})
		return nil
	}

	queries := make([]identityQuery, 0, len(sets))
	byKey := make(map[string]entityCandidateSet, len(sets))
	// keysByName exists only for the verdict loop's fallback: a verdict
	// that names no kind is still attributable when exactly one of the
	// entities we asked about carries that name.
	keysByName := make(map[string][]string, len(sets))
	for _, s := range sets {
		key := entityKey(s.Kind, s.Name)
		if _, seen := byKey[key]; !seen {
			keysByName[s.Name] = append(keysByName[s.Name], key)
		}
		byKey[key] = s
		cands := make([]identityCandidateJSON, 0, len(s.Matches))
		for _, m := range s.Matches {
			cands = append(cands, identityCandidateJSON{ID: m.Entity.ID.String(), Name: m.Entity.Name, Kind: m.Entity.Kind})
		}
		queries = append(queries, identityQuery{Entity: s.Name, Kind: s.Kind, Candidates: cands})
	}
	payload, _ := json.Marshal(map[string]any{"entities": queries})

	captured := &capturedVerdicts{}
	if err := e.LLM.Extract(ctx, llm.DistillInput{
		System:     graphIdentitySystemPrompt,
		User:       string(payload),
		MaxTok:     512,
		Schema:     identityVerdictSchema,
		SchemaName: "anamnesia_identity_verdicts",
	}, captured); err != nil {
		if e.Log != nil {
			e.Log.Warn("extractor: identity disambiguation call failed, creating entities separately", "err", err)
		}
		tr.Step("identity", fmt.Sprintf("%d entities had candidates; the disambiguation call failed, so none merge", len(sets)),
			map[string]any{"entities_with_candidates": len(sets), "error": err.Error()})
		return nil
	}

	affirmed := make(map[string]uuid.UUID)
	var affirmedLog []string
	for _, v := range captured.resp.Verdicts {
		if v.SameAs == "" {
			continue
		}
		name := NormaliseEntityName(v.Entity)
		key := entityKey(NormaliseEntityKind(v.Kind), name)
		set, ok := byKey[key]
		if !ok {
			// No exact (kind, name) match — the model omitted the kind,
			// or wrote one we never asked about. The verdict is still
			// attributable when exactly one of the entities we asked
			// about carries that name; when two do, it is not, and a
			// verdict that cannot be pinned to one entity is dropped
			// rather than applied to the wrong one. Same rule as a
			// same_as id matching none of that entity's own candidates:
			// a fork is visible and fixable, a wrong merge is neither.
			//
			// Second line of defence, not the only one: because recall
			// filters candidates by kind, two same-name sets hold
			// disjoint candidate ids, so the id match below would refuse
			// a misattributed verdict anyway. This makes the rule
			// explicit rather than a consequence of that filter still
			// being there.
			keys := keysByName[name]
			if len(keys) != 1 {
				continue
			}
			key = keys[0]
			set = byKey[key]
		}
		for _, m := range set.Matches {
			if m.Entity.ID.String() == v.SameAs {
				affirmed[key] = m.Entity.ID
				affirmedLog = append(affirmedLog, fmt.Sprintf("%q merged into %q (kind %s, model-affirmed, candidate distance %.4f)",
					set.Name, m.Entity.Name, set.Kind, m.Distance))
				break
			}
		}
		// A same_as that matches none of this entity's own candidates is
		// a hallucinated or mismatched id — dropped, not trusted.
	}
	tr.Step("identity", fmt.Sprintf("%d entities had candidates; the model affirmed %d merges", len(sets), len(affirmed)),
		map[string]any{
			"entities_with_candidates": len(sets),
			"merges_affirmed":          affirmedLog,
			"raw_response":             string(captured.raw),
		})
	return affirmed
}

// embedEntityName is a best-effort embed of one entity's name, so a
// newly created entity carries a vector for a later checkpoint's
// entityCandidatesForName call to find it by. nil on any failure — an
// embedder problem must not fail the graph pass, and UpsertEntity
// already treats a nil Embedding as "no vector yet" rather than an
// error.
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
//
// Name alone, deliberately: an ADD_EDGE endpoint carries no kind, so it
// cannot be keyed the way entities are (see entityKey). A name that is
// ambiguous across kinds is therefore left out of this map entirely by
// the caller, which drops the edge instead of guessing — see runGraph.
func resolveEdges(ops []graphOperation, known map[string]uuid.UUID) (resolved []anamnesia.Edge, dropped []string) {
	for _, op := range ops {
		if strings.ToUpper(strings.TrimSpace(op.Op)) != "ADD_EDGE" {
			continue
		}
		fromID, fromOK := known[NormaliseEntityName(op.From)]
		toID, toOK := known[NormaliseEntityName(op.To)]
		switch {
		case !fromOK && !toOK:
			dropped = append(dropped, fmt.Sprintf("edge %q -> %q (%s): neither endpoint resolved to an entity", op.From, op.To, op.Kind))
		case !fromOK:
			dropped = append(dropped, fmt.Sprintf("edge %q -> %q (%s): %q did not resolve to an entity", op.From, op.To, op.Kind, op.From))
		case !toOK:
			dropped = append(dropped, fmt.Sprintf("edge %q -> %q (%s): %q did not resolve to an entity", op.From, op.To, op.Kind, op.To))
		default:
			// Kind is normalised for the same reason an entity's is:
			// nothing constrains what the model writes, and "depends
			// on", "depends_on" and "Depends On" are one relation
			// wearing three spellings. The retrieval walk passes nil
			// kinds (internal/retrieval/graph.go), so this changes no
			// query — it keeps the graph from describing one edge
			// three ways.
			resolved = append(resolved, anamnesia.Edge{From: fromID, To: toID, Kind: normaliseEdgeKind(op.Kind), Props: op.Props, Trust: op.Trust})
		}
	}
	return resolved, dropped
}

// runGraph is the graph pass for one claude-session-graph source: one
// model call (plus, rarely, a second — see resolveIdentities), then
// entity upserts, edge resolution and edge writes against the store.
func (e *Extractor) runGraph(ctx context.Context, src *anamnesia.Source, tr *activity.Trace) (int, error) {
	cfg := e.Cfg.applyDefaults()
	content := strings.TrimSpace(src.RawContent)
	prompt := e.userPrompt(src, content, nil)

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
			"model":            e.LLM.Model(),
			"latency_ms":       time.Since(started).Milliseconds(),
			"prompt_chars":     len(graphSystemPrompt) + len(prompt),
			"completion_chars": len(captured.raw),
			"raw_response":     string(captured.raw),
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

	// Identity is resolved before any upsert: recall nearby existing
	// entities per extracted name, and — only if that found anything —
	// ask the model to affirm or reject each. See resolveIdentities.
	affirmed := e.resolveIdentities(ctx, src.Scope, ops, tr)

	// Upsert every ADD_ENTITY op, normalised, collecting ids by
	// entityKey — kind AND name, because that is what identifies an
	// entity and two ops in one checkpoint can share a name under
	// different kinds. An op the model just affirmed is the same as an
	// existing entity reuses that entity's id instead of upserting a
	// new one; everything else creates normally, exactly as before any
	// of this existed.
	known := make(map[string]uuid.UUID)
	// idsByName records which entities this checkpoint landed on per
	// bare NAME, for edge endpoints: an ADD_EDGE names entities by name
	// only, with no kind, so it cannot form an entityKey. A name that
	// two kinds share this pass therefore cannot be resolved to one
	// entity, and its edges are dropped rather than pointed at whichever
	// op happened to run last — the rule the LookupEntitiesByName branch
	// below already applies to endpoints from earlier checkpoints, for
	// the same reason: a wrong edge would be believed, a missing one is
	// merely invisible.
	idsByName := make(map[string][]uuid.UUID)
	rememberName := func(name string, id uuid.UUID) {
		for _, existing := range idsByName[name] {
			if existing == id {
				return
			}
		}
		idsByName[name] = append(idsByName[name], id)
	}
	upserted := make([]*anamnesia.Entity, 0, len(ops))
	var merged []string
	failures := make([]string, 0)
	for _, op := range ops {
		if strings.ToUpper(strings.TrimSpace(op.Op)) != "ADD_ENTITY" {
			continue
		}
		name := NormaliseEntityName(op.Name)
		kind := NormaliseEntityKind(op.Kind)
		if name == "" || kind == "" {
			failures = append(failures, fmt.Sprintf("ADD_ENTITY %q: kind and name required", op.Name))
			continue
		}
		key := entityKey(kind, name)
		if id, ok := affirmed[key]; ok {
			known[key] = id
			rememberName(name, id)
			merged = append(merged, name)
			continue
		}
		// op.Props goes in as-is: UpsertEntity merges per key rather than
		// replacing, so a re-declaration carrying no props — the normal
		// case — leaves what an earlier checkpoint recorded alone. Merging
		// here instead would be a read-modify-write two passes could lose.
		ent := &anamnesia.Entity{Scope: src.Scope, Kind: kind, Name: name, Props: op.Props}
		// Attach the name's embedding (best-effort; nil on any failure —
		// UpsertEntity already treats a nil Embedding as "no vector yet")
		// so THIS entity becomes findable as a candidate the next time
		// something nearby is checkpointed. A nil here is not permanent:
		// the worker's embed tick backfills any entity whose embedding is
		// NULL, which is also how entities recover from `migrate --dims N`
		// nulling the whole column (jobs.tickEmbed).
		ent.Embedding = e.embedEntityName(ctx, name)
		if err := e.Store.UpsertEntity(ctx, ent); err != nil {
			if e.Log != nil {
				e.Log.Warn("extractor: upsert entity failed", "err", err)
			}
			failures = append(failures, "ADD_ENTITY "+name+": "+err.Error())
			continue
		}
		known[key] = ent.ID
		rememberName(name, ent.ID)
		upserted = append(upserted, ent)
	}

	// endpoints is what edges resolve against: bare name to one entity
	// id. A name this checkpoint landed on exactly one entity for
	// resolves; one it landed on two (the same name under two kinds)
	// does not, and is reported as ambiguous below when an edge actually
	// names it.
	endpoints := make(map[string]uuid.UUID, len(idsByName))
	for name, ids := range idsByName {
		if len(ids) == 1 {
			endpoints[name] = ids[0]
		}
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
			name := NormaliseEntityName(raw)
			if name == "" || queried[name] {
				continue
			}
			if _, ok := endpoints[name]; ok {
				continue
			}
			queried[name] = true
			if len(idsByName[name]) > 1 {
				// Ambiguous within this checkpoint itself, so there is
				// nothing to look up: the name is not one entity here.
				ambiguous = append(ambiguous, fmt.Sprintf("entity %q is ambiguous: this checkpoint extracted %d entities sharing that name across kinds", name, len(idsByName[name])))
				continue
			}
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
				endpoints[name] = matches[0].ID
			default:
				ambiguous = append(ambiguous, fmt.Sprintf("entity %q is ambiguous: %d entities share that name across kinds", name, len(matches)))
			}
		}
	}

	resolved, dropped := resolveEdges(ops, endpoints)
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
	segmentSources := e.segmentSourceIDsFromMetadata(src.Metadata)
	mentionSources := append([]uuid.UUID{src.ID}, segmentSources...)
	// Every entity this checkpoint touched: the ones its ADD_ENTITY ops
	// landed on (known, one per kind+name — two entities sharing a name
	// each get their own mention), plus any resolved from an earlier
	// checkpoint to carry an edge. Deduped, since an endpoint resolved
	// this pass is usually also in known.
	mentioned := make(map[uuid.UUID]bool, len(known)+len(endpoints))
	for _, id := range known {
		mentioned[id] = true
	}
	for _, id := range endpoints {
		mentioned[id] = true
	}
	// Counted, and counted into the trace: a mention that is never
	// written is the failure this whole bridge already shipped once, and
	// it is invisible from every other number here — entities and edges
	// are written exactly the same either way. A RecordMention error is a
	// failure of the pass, not a log line nobody reads.
	mentionsRecorded := 0
	for id := range mentioned {
		for _, sourceID := range mentionSources {
			if err := e.Store.RecordMention(ctx, id, sourceID); err != nil {
				if e.Log != nil {
					e.Log.Warn("extractor: record mention failed", "entity", id, "source", sourceID, "err", err)
				}
				failures = append(failures, fmt.Sprintf("mention of entity %s on source %s: %s", id, sourceID, err.Error()))
				continue
			}
			mentionsRecorded++
		}
	}

	mentionSourceIDs := make([]string, 0, len(mentionSources))
	for _, id := range mentionSources {
		mentionSourceIDs = append(mentionSourceIDs, id.String())
	}
	tr.Step("graph", fmt.Sprintf("Upserted %d entities, merged %d, created %d edges (%d superseded, %d dropped), recorded %d mentions across %d sources",
		len(upserted), len(merged), edgesCreated, edgesSuperseded, len(dropped), mentionsRecorded, len(mentionSources)),
		map[string]any{
			"entities_upserted": len(upserted),
			"entities_merged":   merged,
			"edges_created":     edgesCreated,
			"edges_superseded":  edgesSuperseded,
			"edges_dropped":     dropped,
			"mentions_recorded": mentionsRecorded,
			"mention_sources":   mentionSourceIDs,
			"segment_sources":   len(segmentSources),
			"failures":          failures,
		})

	executed := len(upserted) + len(merged) + edgesCreated
	if executed == 0 {
		tr.End("skipped", "The model found no durable entities or relationships")
	} else {
		tr.End("ok", fmt.Sprintf("Extracted %d entities and %d edges from a %s %s source",
			len(upserted)+len(merged), edgesCreated, humanBytes(len(content)), src.Kind))
	}
	return executed, nil
}

// segmentSourceIDsFromMetadata reads the checkpoint's segment source ids
// off the graph source's metadata (set by doCheckpoint in cmd/anamnesia/
// hook.go). Tolerant by design: an older checkpoint carries no such key,
// and a graph source posted by something else may not shape it the way
// this expects — either way runGraph must finish with whatever it can
// resolve, not fail the pass over it.
//
// Every path that yields nothing warns, including an absent or empty
// list. Coming back empty is not a shape error, so it used to pass in
// silence — and it is exactly the state the bridge shipped in
// (docs/superpowers/specs/2026-08-19-the-graph-bridge-is-broken.md):
// mentions land only on the graph source, which no search hit ever
// carries, so the graph is populated and unreachable and nothing says
// so.
func (e *Extractor) segmentSourceIDsFromMetadata(meta map[string]any) []uuid.UUID {
	raw, ok := meta["segment_source_ids"]
	if !ok {
		if e.Log != nil {
			e.Log.Warn("extractor: graph source carries no segment_source_ids; mentions will land only on the graph source, which no search hit carries")
		}
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		if e.Log != nil {
			e.Log.Warn("extractor: segment_source_ids metadata has an unexpected shape", "type", fmt.Sprintf("%T", raw))
		}
		return nil
	}
	if len(list) == 0 && e.Log != nil {
		e.Log.Warn("extractor: segment_source_ids is empty; mentions will land only on the graph source, which no search hit carries")
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
