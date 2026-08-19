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
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/llm"
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

	// Upsert every ADD_ENTITY op, normalised, collecting ids by name.
	known := make(map[string]uuid.UUID)
	upserted := make([]*anamnesia.Entity, 0, len(ops))
	kindsSeen := make(map[string]bool)
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
		if err := e.Store.UpsertEntity(ctx, ent); err != nil {
			if e.Log != nil {
				e.Log.Warn("extractor: upsert entity failed", "err", err)
			}
			failures = append(failures, "ADD_ENTITY "+name+": "+err.Error())
			continue
		}
		known[name] = ent.ID
		upserted = append(upserted, ent)
		kindsSeen[op.Kind] = true
	}

	// Resolve edge endpoints not created this pass against entities from
	// an earlier checkpoint. LookupEntity needs a kind, which a bare
	// edge endpoint name does not carry, so the kinds this pass itself
	// declared via ADD_ENTITY are the candidate vocabulary tried.
	for _, op := range ops {
		if strings.ToUpper(strings.TrimSpace(op.Op)) != "ADD_EDGE" {
			continue
		}
		for _, raw := range []string{op.From, op.To} {
			name := normaliseEntityName(raw)
			if name == "" {
				continue
			}
			if _, ok := known[name]; ok {
				continue
			}
			for kind := range kindsSeen {
				if ent, err := e.Store.LookupEntity(ctx, src.Scope, kind, name); err == nil {
					known[name] = ent.ID
					break
				}
			}
		}
	}

	resolved, dropped := resolveEdges(ops, known)

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

	for _, id := range known {
		if err := e.Store.RecordMention(ctx, id, src.ID); err != nil && e.Log != nil {
			e.Log.Warn("extractor: record mention failed", "err", err)
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
