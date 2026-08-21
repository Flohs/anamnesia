// consolidate.go: clusters of similar experiences are distilled into
// higher-abstraction insights with the LLM. Run from the worker loop.
//
// The clustering is a single greedy pass over candidates sorted by
// last_used_at desc, assigning each row to the first cluster whose
// centroid cosine-sim ≥ threshold. New rows that don't fit any existing
// cluster open a new one. Clusters cap at MaxCluster members so a hot
// vein of similar experiences doesn't produce a 50-source distillation
// the LLM can't reason about coherently.

package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/activity"
	"github.com/flohs/anamnesia/internal/llm"
	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// Defaults for the clustering pass. SimThreshold is the one that
// matters, and it shipped at 0.85, which no real corpus reaches.
//
// Measured on a live install on 2026-08-21, over the 1,402 same-scope
// pairs of experiences one user owned: the mean cosine was 0.289, the
// single most similar pair scored 0.754, and NOTHING cleared 0.85. So no
// cluster of two ever formed, the LLM was never called, and every pass
// finished in ~42ms reporting success while folding nothing. The only
// output it had ever produced came from the `doctor` health check, whose
// rows are byte-identical and score 1.000.
//
// 0.65 was picked by replaying this same greedy algorithm over that
// corpus at each candidate threshold and reading what it merged. It
// forms 5 clusters, all clean topical pairs ("Managed app deletion
// finalization" with "Managed App Deletion Design"). 0.60 also merges
// well but fills a cluster to MaxCluster, and an over-merged summary is
// worse than a missing one: it competes in retrieval with the sources it
// blurred, while a cluster left unmerged is simply picked up later.
const (
	DefaultConsolidateSimilarity = 0.65
	DefaultConsolidateMaxCluster = 8
)

// ConsolidateConfig tunes the clustering + distillation pass.
type ConsolidateConfig struct {
	Window       time.Duration // lookback for "recent experiences" (default 7d)
	SimThreshold float64       // cosine threshold for cluster merge (default DefaultConsolidateSimilarity)
	MaxCluster   int           // cap cluster size (default DefaultConsolidateMaxCluster)
	MinCluster   int           // gates which clusters bother the LLM (default 2)
	BatchLimit   int           // max experiences to fetch per scope per tick (default 200)
}

type cluster struct {
	members  []*anamnesia.Experience
	centroid []float32
}

// ConsolidationRun executes one consolidation pass across every active
// (user, project) scope touched in the last `since` window.
func ConsolidationRun(ctx context.Context, st *store.Store, lm llm.Client, cfg ConsolidateConfig, log *slog.Logger, since time.Duration, rec *activity.Recorder) error {
	cfg = applyConsolidateDefaults(cfg)
	if since <= 0 {
		since = cfg.Window
	}

	scopes, err := activeScopes(ctx, st, since)
	if err != nil {
		return fmt.Errorf("active scopes: %w", err)
	}
	// One trace per pass. Consolidation is the least visible thing the
	// server does and the most destructive-looking: it supersedes rows
	// the user wrote. Being able to read why is the point.
	tr := rec.Begin("consolidate", "", "")
	tr.Step("scopes", fmt.Sprintf("%d scopes active in the last %s", len(scopes), since),
		map[string]any{"scopes": scopeDetails(ctx, st, scopes), "window": since.String()})
	if len(scopes) == 0 {
		tr.End("skipped", "Nothing has been written recently enough to consolidate")
		return nil
	}
	written := 0
	for _, sc := range scopes {
		n, err := consolidateScope(ctx, st, lm, cfg, log, sc, tr)
		written += n
		if err != nil {
			if log != nil {
				log.Warn("consolidate scope failed",
					"user_id", sc.UserID, "project_id", sc.ProjectID, "err", err)
			}
			tr.Fail("scope", err)
			// Continue with other scopes — one bad scope shouldn't block
			// the rest.
		}
	}
	if written == 0 {
		tr.End("skipped", "No cluster was large enough to distil")
	} else {
		tr.End("ok", fmt.Sprintf("Distilled %d new insights", written))
	}
	return nil
}

// scopeDetails labels the scopes a pass covers. Consolidation runs
// server-wide, so a trace that only carried ids would need a lookup per
// row to be readable.
func scopeDetails(ctx context.Context, st *store.Store, scopes []anamnesia.Scope) []map[string]any {
	out := make([]map[string]any, 0, len(scopes))
	for _, sc := range scopes {
		entry := map[string]any{"user_id": sc.UserID.String()}
		if handle, err := st.LookupUserHandle(ctx, sc.UserID); err == nil {
			entry["user"] = handle
		}
		if sc.ProjectID != nil {
			entry["project_id"] = sc.ProjectID.String()
			if slug, err := st.LookupProjectSlug(ctx, *sc.ProjectID); err == nil {
				entry["project"] = slug
			}
		}
		out = append(out, entry)
	}
	return out
}

// activeScopes returns the (user_id, project_id) pairs that have any
// experience touched in the last `since` window.
func activeScopes(ctx context.Context, st *store.Store, since time.Duration) ([]anamnesia.Scope, error) {
	rows, err := st.Pool.Query(ctx, `
		SELECT DISTINCT user_id, project_id
		FROM experiences
		WHERE deleted_at IS NULL
		  AND last_used_at > now() - $1::interval`, since.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []anamnesia.Scope
	for rows.Next() {
		var (
			uid uuid.UUID
			pid *uuid.UUID
		)
		if err := rows.Scan(&uid, &pid); err != nil {
			return nil, err
		}
		out = append(out, anamnesia.Scope{UserID: uid, ProjectID: pid})
	}
	return out, rows.Err()
}

func consolidateScope(ctx context.Context, st *store.Store, lm llm.Client, cfg ConsolidateConfig, log *slog.Logger, scope anamnesia.Scope, tr *activity.Trace) (int, error) {
	// Only consolidate abstraction=0 (raw trajectories). Higher levels
	// are already distilled.
	cands, err := candidatesForConsolidation(ctx, st, scope, cfg.Window, cfg.BatchLimit)
	if err != nil {
		return 0, err
	}
	if len(cands) < cfg.MinCluster {
		return 0, nil
	}
	clusters := buildClusters(cands, cfg.SimThreshold, cfg.MaxCluster)
	done, err := distilledMemberSets(ctx, st, scope)
	if err != nil {
		return 0, err
	}
	eligible, skipped := 0, 0
	for _, cl := range clusters {
		if len(cl.members) < cfg.MinCluster {
			continue
		}
		eligible++
		if done[clusterKey(cl.members)] {
			skipped++
		}
	}
	tr.Step("cluster", fmt.Sprintf("Formed %d clusters from %d experiences", eligible, len(cands)),
		map[string]any{
			"candidates": len(cands),
			"threshold":  cfg.SimThreshold,
			"clusters":   clusterDetails(clusters, cfg.MinCluster),
			// Skipping is the ordinary steady state once a scope has
			// been consolidated, so a pass that folds nothing has to say
			// whether it found nothing or had already done the work.
			"already_distilled": skipped,
		})
	written := 0
	for i, cl := range clusters {
		if len(cl.members) < cfg.MinCluster {
			continue
		}
		if done[clusterKey(cl.members)] {
			continue
		}
		if err := distilCluster(ctx, st, lm, scope, cl, log, tr, i); err != nil {
			return written, err
		}
		done[clusterKey(cl.members)] = true
		written++
	}
	return written, nil
}

// clusterDetails renders the clusters a pass formed, with the similarity
// that held each one together. Only the ones that will be distilled: a
// cluster of one is the ordinary case and would drown the rest.
func clusterDetails(clusters []cluster, minCluster int) []map[string]any {
	out := make([]map[string]any, 0, len(clusters))
	for i, cl := range clusters {
		if len(cl.members) < minCluster {
			continue
		}
		members := make([]map[string]any, 0, len(cl.members))
		lowest := 1.0
		for _, m := range cl.members {
			members = append(members, map[string]any{"id": m.ID.String(), "title": m.Title})
			if sim := cosine(cl.centroid, m.Embedding); sim < lowest {
				lowest = sim
			}
		}
		out = append(out, map[string]any{
			"index":               i,
			"members":             members,
			"centroid_similarity": lowest,
		})
	}
	return out
}

// candidatesForConsolidation pulls abstraction=0 experiences in scope
// that have an embedding and are still live (not superseded, not deleted).
func candidatesForConsolidation(ctx context.Context, st *store.Store, scope anamnesia.Scope, window time.Duration, limit int) ([]*anamnesia.Experience, error) {
	args := []any{scope.UserID}
	where := []string{"user_id = $1"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("project_id = $%d", len(args)))
	} else {
		// activeScopes groups by (user_id, project_id), so a nil project
		// is the scope of the user's project-less rows — not "any
		// project". Omitting the filter here made a single project-less
		// experience pull in every project the user had, cluster them
		// together, and write the summary at project_id NULL, which
		// retrieval returns in every project.
		where = append(where, "project_id IS NULL")
	}
	args = append(args, window.String())
	where = append(where,
		fmt.Sprintf("last_used_at > now() - $%d::interval", len(args)),
		"deleted_at IS NULL",
		"invalidated_at IS NULL",
		"superseded_by IS NULL",
		"abstraction = 0",
		"embedding IS NOT NULL")
	args = append(args, limit)
	q := `SELECT id, user_id, project_id, source_id, kind, abstraction, title, body, outcome, meta,
		trust, importance, relevance, pii_tags, use_count, last_used_at, embed_model,
		valid_from, valid_to, ingested_at, invalidated_at, superseded_by, deleted_at,
		occurred_at, participants, topic, parent_id, provenance,
		embedding
		FROM experiences WHERE ` + joinAnd(where) +
		fmt.Sprintf(` ORDER BY last_used_at DESC LIMIT $%d`, len(args))
	rows, err := st.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*anamnesia.Experience
	for rows.Next() {
		e, vec, err := scanExperienceWithEmbedding(rows)
		if err != nil {
			return nil, err
		}
		e.Embedding = vec
		out = append(out, e)
	}
	return out, rows.Err()
}

// clusterKey canonicalises a cluster's membership as its sorted member
// ids. Two passes over the same rows produce the same key whatever order
// the candidates came back in.
func clusterKey(members []*anamnesia.Experience) string {
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.ID.String()
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// distilledMemberSets returns the membership of every cluster this scope
// has already distilled, keyed by clusterKey.
//
// Consolidation deliberately does NOT supersede its sources: doing that
// once invalidated every source row and silently broke fact-grounded
// retrieval (see distilCluster). But nothing replaced that guard, so the
// sources stayed eligible forever and every pass distilled the same
// cluster again — on a live install, 8 rows had become 13 summaries
// across 63 source links and were still growing. This is the
// replacement: additive like the rest of the pass, and keyed on what a
// summary was built from rather than on the source rows themselves, so
// nothing is mutated to record that the work was done.
//
// The scope filter mirrors candidatesForConsolidation, including its
// treatment of a nil ProjectID, so the two halves always consider the
// same rows.
func distilledMemberSets(ctx context.Context, st *store.Store, scope anamnesia.Scope) (map[string]bool, error) {
	args := []any{scope.UserID}
	where := []string{"user_id = $1"}
	if scope.ProjectID != nil {
		args = append(args, *scope.ProjectID)
		where = append(where, fmt.Sprintf("project_id = $%d", len(args)))
	} else {
		where = append(where, "project_id IS NULL")
	}
	where = append(where,
		"abstraction > 0",
		"deleted_at IS NULL",
		// -> yields NULL for a missing key and for a NULL meta, which
		// avoids the jsonb ? operator entirely.
		"meta->'consolidated_from' IS NOT NULL")
	rows, err := st.Pool.Query(ctx,
		`SELECT meta->'consolidated_from' FROM experiences WHERE `+joinAnd(where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var ids []string
		if err := json.Unmarshal(raw, &ids); err != nil {
			// A summary whose lineage we cannot read cannot vouch for a
			// cluster. Skipping it risks one duplicate; failing the pass
			// would stop consolidation for the whole scope.
			continue
		}
		sort.Strings(ids)
		out[strings.Join(ids, ",")] = true
	}
	return out, rows.Err()
}

func joinAnd(parts []string) string {
	s := parts[0]
	for _, p := range parts[1:] {
		s += " AND " + p
	}
	return s
}

// scanExperienceWithEmbedding mirrors store.scanExperience but also reads
// the embedding column at the end of the row. Kept in the jobs package
// so the store API surface stays narrow (consolidation is the only
// caller that needs to see the raw vector).
func scanExperienceWithEmbedding(row interface {
	Scan(...any) error
}) (*anamnesia.Experience, []float32, error) {
	var (
		e            anamnesia.Experience
		project      *uuid.UUID
		sourceID     *uuid.UUID
		title        *string
		outcome      *string
		metaJSON     []byte
		piiTags      []string
		embMod       *string
		validTo      *time.Time
		invalid      *time.Time
		superBy      *uuid.UUID
		deleted      *time.Time
		occurredAt   *time.Time
		participants []string
		topic        *string
		parentID     *uuid.UUID
		provJSON     []byte
		embRaw       any
	)
	err := row.Scan(
		&e.ID, &e.Scope.UserID, &project, &sourceID, &e.Kind, &e.Abstraction,
		&title, &e.Body, &outcome, &metaJSON,
		&e.Trust, &e.Importance, &e.Relevance, &piiTags, &e.UseCount, &e.LastUsedAt, &embMod,
		&e.ValidFrom, &validTo, &e.IngestedAt, &invalid, &superBy, &deleted,
		&occurredAt, &participants, &topic, &parentID, &provJSON,
		&embRaw,
	)
	if err != nil {
		return nil, nil, err
	}
	e.Scope.ProjectID = project
	e.SourceID = sourceID
	if title != nil {
		e.Title = *title
	}
	if outcome != nil {
		e.Outcome = anamnesia.Outcome(*outcome)
	}
	if embMod != nil {
		e.EmbedModel = *embMod
	}
	e.PIITags = piiTags
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &e.Meta)
	}
	e.ValidTo = validTo
	e.InvalidatedAt = invalid
	e.SupersededBy = superBy
	e.DeletedAt = deleted
	e.OccurredAt = occurredAt
	e.Participants = participants
	if topic != nil {
		e.Topic = *topic
	}
	e.ParentID = parentID
	if len(provJSON) > 0 {
		_ = json.Unmarshal(provJSON, &e.Provenance)
	}
	vec := decodeVector(embRaw)
	return &e, vec, nil
}

// decodeVector handles the two shapes pgx might return for a pgvector
// column when the column isn't pre-registered as pgvector.Vector:
// [...]byte for the text wire format ("[0.1,0.2,...]") or a
// pgvector.Vector itself if the type was registered.
func decodeVector(v any) []float32 {
	switch x := v.(type) {
	case []byte:
		return parseVectorText(string(x))
	case string:
		return parseVectorText(x)
	}
	return nil
}

func parseVectorText(s string) []float32 {
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil
	}
	s = s[1 : len(s)-1]
	if s == "" {
		return nil
	}
	var out []float32
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			var f float32
			_, _ = fmt.Sscanf(s[start:i], "%f", &f)
			out = append(out, f)
			start = i + 1
		}
	}
	return out
}

// buildClusters runs the greedy single-pass agglomerative clustering.
func buildClusters(cands []*anamnesia.Experience, threshold float64, maxCluster int) []cluster {
	var out []cluster
	for _, c := range cands {
		if len(c.Embedding) == 0 {
			continue
		}
		assigned := false
		for i := range out {
			if len(out[i].members) >= maxCluster {
				continue
			}
			if cosine(out[i].centroid, c.Embedding) >= threshold {
				out[i].members = append(out[i].members, c)
				out[i].centroid = updateCentroid(out[i].centroid, c.Embedding, len(out[i].members))
				assigned = true
				break
			}
		}
		if !assigned {
			out = append(out, cluster{
				members:  []*anamnesia.Experience{c},
				centroid: normalise(copyVec(c.Embedding)),
			})
		}
	}
	return out
}

const consolidateSystemPrompt = `You are the consolidation worker for an agent memory system. You receive a small cluster of similar experience records and distil them into one higher-abstraction summary.

Rules:
- title: concise (<= 80 chars), action- or insight-oriented.
- body: a single distilled summary, 2-6 sentences. Do not list each source.
- outcome: pick the modal outcome across sources ("success", "failure", or "partial"). Use "" only if every source has no outcome.
- importance: number between 0.1 and 1.0 inclusive; reflect how broadly useful the distilled insight is.
- kind: "case" if the sources are concrete trajectories, "strategy" if they generalise to a reusable workflow or insight, "hybrid" if a mix.

Respond ONLY with valid JSON matching this schema:
{"title":"...","body":"...","outcome":"success|failure|partial|","importance":0.0-1.0,"kind":"case|strategy|hybrid"}`

type distilled struct {
	Title      string  `json:"title"`
	Body       string  `json:"body"`
	Outcome    string  `json:"outcome"`
	Importance float32 `json:"importance"`
	Kind       string  `json:"kind"`
}

// distilCluster builds the LLM prompt, calls Distill, persists the
// distilled record as abstraction=1, and supersedes the source rows.
func distilCluster(ctx context.Context, st *store.Store, lm llm.Client, scope anamnesia.Scope, cl cluster, log *slog.Logger, tr *activity.Trace, index int) error {
	type srcRow struct {
		ID         string  `json:"id"`
		Title      string  `json:"title,omitempty"`
		Body       string  `json:"body"`
		Outcome    string  `json:"outcome,omitempty"`
		Importance float32 `json:"importance"`
	}
	srcs := make([]srcRow, 0, len(cl.members))
	for _, m := range cl.members {
		srcs = append(srcs, srcRow{
			ID: m.ID.String(), Title: m.Title, Body: m.Body,
			Outcome: string(m.Outcome), Importance: m.Importance,
		})
	}
	userJSON, err := json.Marshal(srcs)
	if err != nil {
		return fmt.Errorf("marshal user payload: %w", err)
	}
	var out distilled
	started := time.Now()
	if err := lm.Distill(ctx, llm.DistillInput{
		System: consolidateSystemPrompt,
		User:   string(userJSON),
		MaxTok: 1024,
	}, &out); err != nil {
		tr.Fail("distil", err)
		return fmt.Errorf("llm: %w", err)
	}
	tr.Step("distil", fmt.Sprintf("Distilled %d experiences into one insight", len(cl.members)),
		map[string]any{
			"cluster_index": index,
			"model":         lm.Model(),
			"latency_ms":    time.Since(started).Milliseconds(),
			"result_title":  out.Title,
			"result_body":   out.Body,
		})
	kind := anamnesia.ExperienceKind(out.Kind)
	if !kind.Valid() {
		kind = inferKindFromCluster(cl.members)
	}
	imp := clampFloat(out.Importance, 0.1, 1.0)
	if imp == 0 {
		imp = 0.5
	}
	newExp := &anamnesia.Experience{
		Scope:       scope,
		Kind:        kind,
		Abstraction: 1,
		Title:       out.Title,
		Body:        out.Body,
		Outcome:     anamnesia.Outcome(out.Outcome),
		Importance:  imp,
		Trust:       0.7,
		// parent_id links the summary to one representative source
		// experience; the full cluster lineage lives in meta.
		ParentID: &cl.members[0].ID,
		Meta: map[string]any{
			"consolidated_from": collectIDs(cl.members),
			"consolidated_at":   time.Now().UTC(),
			"abstraction":       1,
		},
	}
	if err := st.RecordExperience(ctx, newExp); err != nil {
		tr.Fail("write", err)
		return fmt.Errorf("record distilled: %w", err)
	}
	// Consolidation is additive, so the trace says what was added and
	// what it was derived from, and never implies the sources went away.
	tr.Step("write", fmt.Sprintf("Wrote 1 abstraction-1 experience from %d sources", len(cl.members)),
		map[string]any{
			"written": []map[string]any{{
				"target":      "experience",
				"id":          newExp.ID.String(),
				"abstraction": 1,
			}},
			"derived_from": collectIDs(cl.members),
		})
	// Note: source experiences (abstraction=0) are NOT superseded here.
	// Consolidation is additive — the summary is a derived layer for
	// callers that want a thematic overview; the sources remain active
	// so callers that need verbatim evidence still see them. Earlier
	// versions called SupersedeExperience here, which invalidated every
	// source row and silently broke fact-grounded retrieval. See
	// retrieval.Query.OnlyRaw for the abstraction filter on the read path.
	if log != nil {
		log.Info("consolidated cluster",
			"distilled", newExp.ID,
			"sources", len(cl.members),
			"user", scope.UserID,
			"project", scope.ProjectID)
	}
	return nil
}

func collectIDs(members []*anamnesia.Experience) []string {
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.ID.String()
	}
	return ids
}

func inferKindFromCluster(members []*anamnesia.Experience) anamnesia.ExperienceKind {
	outcomes := map[anamnesia.Outcome]int{}
	for _, m := range members {
		outcomes[m.Outcome]++
	}
	if len(outcomes) > 1 {
		return anamnesia.ExperienceStrategy
	}
	first := members[0].Kind
	for _, m := range members[1:] {
		if m.Kind != first {
			return anamnesia.ExperienceHybrid
		}
	}
	if !first.Valid() {
		return anamnesia.ExperienceCase
	}
	return first
}

func applyConsolidateDefaults(c ConsolidateConfig) ConsolidateConfig {
	if c.Window == 0 {
		c.Window = 7 * 24 * time.Hour
	}
	if c.SimThreshold == 0 {
		c.SimThreshold = DefaultConsolidateSimilarity
	}
	if c.MaxCluster == 0 {
		c.MaxCluster = DefaultConsolidateMaxCluster
	}
	if c.MinCluster == 0 {
		c.MinCluster = 2
	}
	if c.BatchLimit == 0 {
		c.BatchLimit = 200
	}
	return c
}

// ─── math helpers ────────────────────────────────────────────────────

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func updateCentroid(centroid, newVec []float32, n int) []float32 {
	if len(centroid) != len(newVec) || n <= 0 {
		return centroid
	}
	w := float32(n - 1)
	out := make([]float32, len(centroid))
	for i := range centroid {
		out[i] = (centroid[i]*w + newVec[i]) / float32(n)
	}
	return normalise(out)
}

func normalise(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := 1.0 / math.Sqrt(sum)
	for i := range v {
		v[i] = float32(float64(v[i]) * inv)
	}
	return v
}

func copyVec(v []float32) []float32 {
	out := make([]float32, len(v))
	copy(out, v)
	return out
}

func clampFloat(v, lo, hi float32) float32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
