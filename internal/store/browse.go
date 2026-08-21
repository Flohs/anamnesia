// browse.go is the read API's way through the memory tables: filtered,
// ordered and paged, one function per domain.
//
// Paging is keyset rather than offset. A UI polling page 2 of a list
// that is still being written to would otherwise see rows shift under
// it: an offset counts from a position that moves, while (timestamp, id)
// names a row that does not. The cursor is that pair, opaque so it can
// change shape later.
//
// The existing List* methods stay as they are. They serve the hooks and
// MCP, which want small fixed sets rather than pages.
package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// browseDefaultLimit and browseMaxLimit bound a page. The cap is a
// ceiling on what one request can cost, not an error: an absurd limit is
// answered with the maximum rather than a refusal.
const (
	browseDefaultLimit = 50
	browseMaxLimit     = 200
)

// Browse is the filter set for a listing. Every domain reads the fields
// that apply to it and ignores the rest, which is what lets one HTTP
// layer serve seven lists without seven parameter structs.
type Browse struct {
	Scope  anamnesia.Scope
	Q      string // case-insensitive substring over the domain's text
	Limit  int
	Cursor string

	// facts
	FactScope      string
	IncludeDeleted bool
	// experiences, entities, edges
	Kind        string
	Abstraction *int
	ParentID    *uuid.UUID
	Topic       string
	Since       *time.Time
	Until       *time.Time
	FromID      *uuid.UUID
	ToID        *uuid.UUID
	// sources
	State string
	// working memory
	SessionID *uuid.UUID
}

// qb numbers placeholders as they are added, so a clause never has to
// know how many arguments came before it.
type qb struct{ args []any }

func (b *qb) ph(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

// scoped starts the where clause with the standard scope predicate.
func (b *qb) scoped(scope anamnesia.Scope, prefix string) []string {
	where := []string{prefix + "user_id = " + b.ph(scope.UserID)}
	if scope.ProjectID != nil {
		where = append(where, "("+prefix+"project_id = "+b.ph(*scope.ProjectID)+
			" OR "+prefix+"project_id IS NULL)")
	}
	return where
}

// like adds a case-insensitive substring predicate over several columns.
func (b *qb) like(where []string, q string, columns ...string) []string {
	if strings.TrimSpace(q) == "" {
		return where
	}
	p := b.ph("%" + q + "%")
	parts := make([]string, 0, len(columns))
	for _, c := range columns {
		parts = append(parts, c+" ILIKE "+p)
	}
	return append(where, "("+strings.Join(parts, " OR ")+")")
}

// tail closes a browse query with the cursor predicate, the ordering and
// a limit one larger than asked for: that extra row is how the query
// knows whether there is another page without counting the whole table.
func (b Browse) tail(q *qb, where []string, tsColumn, idColumn string) (string, int, error) {
	if b.Cursor != "" {
		at, id, err := decodeCursor(b.Cursor)
		if err != nil {
			return "", 0, err
		}
		where = append(where, "("+tsColumn+", "+idColumn+") < ("+q.ph(at)+", "+q.ph(id)+")")
	}
	limit := b.Limit
	if limit <= 0 {
		limit = browseDefaultLimit
	}
	if limit > browseMaxLimit {
		limit = browseMaxLimit
	}
	return fmt.Sprintf(" WHERE %s ORDER BY %s DESC, %s DESC LIMIT %s",
		strings.Join(where, " AND "), tsColumn, idColumn, q.ph(limit+1)), limit, nil
}

// encodeCursor names a row by its sort key. Opaque on purpose: callers
// pass it back rather than construct it.
func encodeCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

// ErrBadCursor is a cursor this server cannot read. It is the caller's
// mistake rather than the server's, so the HTTP layer answers 400.
var ErrBadCursor = errors.New("cursor is not a cursor")

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: %s", ErrBadCursor, err)
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: no separator", ErrBadCursor)
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: bad timestamp", ErrBadCursor)
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: bad id", ErrBadCursor)
	}
	return t, u, nil
}

const factSelect = `SELECT id, user_id, project_id, source_id, fact_scope, key, value, source, trust, pii_tags,
	embed_model, valid_from, valid_to, ingested_at, invalidated_at, superseded_by, deleted_at`

// BrowseFacts lists facts newest first.
func (s *Store) BrowseFacts(ctx context.Context, b Browse) ([]*anamnesia.Fact, string, error) {
	var q qb
	where := q.scoped(b.Scope, "")
	if !b.IncludeDeleted {
		where = append(where, "deleted_at IS NULL")
		// Superseded values are history, not memory. They are reachable
		// through retrieval with include_history.
		where = append(where, "superseded_by IS NULL")
	}
	if b.FactScope != "" {
		where = append(where, "fact_scope = "+q.ph(b.FactScope))
	}
	where = q.like(where, b.Q, "key", "value::text")
	tail, limit, err := b.tail(&q, where, "ingested_at", "id")
	if err != nil {
		return nil, "", err
	}
	rows, err := s.Pool.Query(ctx, factSelect+" FROM facts"+tail, q.args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []*anamnesia.Fact{}
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		return out, encodeCursor(last.IngestedAt, last.ID), nil
	}
	return out, "", nil
}

// BrowseExperiences lists experiences newest first.
func (s *Store) BrowseExperiences(ctx context.Context, b Browse) ([]*anamnesia.Experience, string, error) {
	var q qb
	where := q.scoped(b.Scope, "")
	if !b.IncludeDeleted {
		where = append(where, "deleted_at IS NULL")
	}
	if b.Kind != "" {
		where = append(where, "kind = "+q.ph(b.Kind))
	}
	if b.Abstraction != nil {
		where = append(where, "abstraction = "+q.ph(*b.Abstraction))
	}
	if b.ParentID != nil {
		where = append(where, "parent_id = "+q.ph(*b.ParentID))
	}
	if b.Topic != "" {
		where = append(where, "topic = "+q.ph(b.Topic))
	}
	if b.Since != nil {
		where = append(where, "coalesce(occurred_at, ingested_at) >= "+q.ph(*b.Since))
	}
	if b.Until != nil {
		where = append(where, "coalesce(occurred_at, ingested_at) < "+q.ph(*b.Until))
	}
	where = q.like(where, b.Q, "title", "body", "topic")
	tail, limit, err := b.tail(&q, where, "ingested_at", "id")
	if err != nil {
		return nil, "", err
	}
	rows, err := s.Pool.Query(ctx, expSelectCols+" FROM experiences"+tail, q.args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []*anamnesia.Experience{}
	for rows.Next() {
		e, err := scanExperience(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		return out, encodeCursor(last.IngestedAt, last.ID), nil
	}
	return out, "", nil
}

const skillSelect = `SELECT id, user_id, project_id, name, kind, description, signature, body, meta,
	use_count, last_used_at`

// BrowseSkills lists skills newest first.
func (s *Store) BrowseSkills(ctx context.Context, b Browse) ([]*anamnesia.Skill, string, error) {
	var q qb
	where := q.scoped(b.Scope, "")
	if !b.IncludeDeleted {
		where = append(where, "deleted_at IS NULL")
	}
	if b.Kind != "" {
		where = append(where, "kind = "+q.ph(b.Kind))
	}
	where = q.like(where, b.Q, "name", "description")
	tail, limit, err := b.tail(&q, where, "created_at", "id")
	if err != nil {
		return nil, "", err
	}
	rows, err := s.Pool.Query(ctx, skillSelect+", created_at FROM skills"+tail, q.args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []*anamnesia.Skill{}
	var created []time.Time
	for rows.Next() {
		var at time.Time
		sk, err := scanSkill(rowPlus{row: rows, extra: []any{&at}})
		if err != nil {
			return nil, "", err
		}
		out = append(out, sk)
		created = append(created, at)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(out) > limit {
		out = out[:limit]
		return out, encodeCursor(created[limit-1], out[limit-1].ID), nil
	}
	return out, "", nil
}

const entitySelect = `SELECT id, user_id, project_id, kind, name, props, created_at`

// BrowseEntities lists graph entities newest first.
func (s *Store) BrowseEntities(ctx context.Context, b Browse) ([]*anamnesia.Entity, string, error) {
	var q qb
	where := q.scoped(b.Scope, "")
	if b.Kind != "" {
		where = append(where, "kind = "+q.ph(b.Kind))
	}
	where = q.like(where, b.Q, "name", "kind")
	tail, limit, err := b.tail(&q, where, "created_at", "id")
	if err != nil {
		return nil, "", err
	}
	rows, err := s.Pool.Query(ctx, entitySelect+" FROM entities"+tail, q.args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []*anamnesia.Entity{}
	for rows.Next() {
		e, err := scanEntity(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		return out, encodeCursor(last.CreatedAt, last.ID), nil
	}
	return out, "", nil
}

const edgeSelect = `SELECT e.id, e.from_id, e.to_id, e.kind, e.props, e.valid_from, e.valid_to,
	e.ingested_at, e.invalidated_at, e.source, e.trust`

// BrowseEdges lists graph edges newest first.
//
// Edges carry no scope of their own, so this joins the entity an edge
// starts from and scopes that. An edge whose endpoints belong to someone
// else is therefore invisible, which is the only reading that makes the
// graph obey the same boundary as everything else.
func (s *Store) BrowseEdges(ctx context.Context, b Browse) ([]*anamnesia.Edge, string, error) {
	var q qb
	where := q.scoped(b.Scope, "en.")
	if !b.IncludeDeleted {
		where = append(where, "e.invalidated_at IS NULL")
	}
	if b.Kind != "" {
		where = append(where, "e.kind = "+q.ph(b.Kind))
	}
	if b.FromID != nil {
		where = append(where, "e.from_id = "+q.ph(*b.FromID))
	}
	if b.ToID != nil {
		where = append(where, "e.to_id = "+q.ph(*b.ToID))
	}
	where = q.like(where, b.Q, "e.kind")
	tail, limit, err := b.tail(&q, where, "e.ingested_at", "e.id")
	if err != nil {
		return nil, "", err
	}
	rows, err := s.Pool.Query(ctx,
		edgeSelect+" FROM edges e JOIN entities en ON en.id = e.from_id"+tail, q.args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []*anamnesia.Edge{}
	for rows.Next() {
		e, err := scanEdge(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		return out, encodeCursor(last.IngestedAt, last.ID), nil
	}
	return out, "", nil
}

// BrowseSources lists ingested sources newest first.
func (s *Store) BrowseSources(ctx context.Context, b Browse) ([]*anamnesia.Source, string, error) {
	var q qb
	where := q.scoped(b.Scope, "")
	if b.State != "" {
		where = append(where, "extraction_state = "+q.ph(b.State))
	}
	if b.Kind != "" {
		where = append(where, "kind = "+q.ph(b.Kind))
	}
	if b.Since != nil {
		where = append(where, "occurred_at >= "+q.ph(*b.Since))
	}
	if b.Until != nil {
		where = append(where, "occurred_at < "+q.ph(*b.Until))
	}
	where = q.like(where, b.Q, "title", "kind", "raw_content")
	tail, limit, err := b.tail(&q, where, "ingested_at", "id")
	if err != nil {
		return nil, "", err
	}
	rows, err := s.Pool.Query(ctx, sourceSelect+" FROM sources"+tail, q.args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []*anamnesia.Source{}
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		return out, encodeCursor(last.IngestedAt, last.ID), nil
	}
	return out, "", nil
}

const workingSelect = `SELECT id, user_id, project_id, session_id, position, role, body, meta,
	folded_into, expires_at, created_at`

// BrowseWorking lists in-session working memory newest first.
func (s *Store) BrowseWorking(ctx context.Context, b Browse) ([]*anamnesia.WorkingEntry, string, error) {
	var q qb
	where := q.scoped(b.Scope, "")
	if b.SessionID != nil {
		where = append(where, "session_id = "+q.ph(*b.SessionID))
	}
	where = q.like(where, b.Q, "body", "role")
	tail, limit, err := b.tail(&q, where, "created_at", "id")
	if err != nil {
		return nil, "", err
	}
	rows, err := s.Pool.Query(ctx, workingSelect+" FROM working_memory"+tail, q.args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []*anamnesia.WorkingEntry{}
	for rows.Next() {
		wm, err := scanWorking(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, wm)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		return out, encodeCursor(last.CreatedAt, last.ID), nil
	}
	return out, "", nil
}

// rowPlus lets a scanner written for one column list read a query that
// selects extra columns after it. Skills carry a created_at in the table
// but not on the type, and the cursor needs it: this is cheaper than a
// second copy of the skill scanner that would then have to be kept in
// step with the first.
type rowPlus struct {
	row   rowScanner
	extra []any
}

func (r rowPlus) Scan(dest ...any) error {
	return r.row.Scan(append(dest, r.extra...)...)
}

func scanEdge(row rowScanner) (*anamnesia.Edge, error) {
	var (
		e          anamnesia.Edge
		propsRaw   []byte
		validTo    *time.Time
		invalidate *time.Time
	)
	if err := row.Scan(
		&e.ID, &e.From, &e.To, &e.Kind, &propsRaw, &e.ValidFrom, &validTo,
		&e.IngestedAt, &invalidate, &e.Source, &e.Trust,
	); err != nil {
		return nil, err
	}
	if len(propsRaw) > 0 {
		_ = json.Unmarshal(propsRaw, &e.Props)
	}
	e.ValidTo = validTo
	e.InvalidatedAt = invalidate
	return &e, nil
}

// ─── single rows ─────────────────────────────────────────────────────
//
// The read API links a list row to its detail page, so every domain it
// lists needs a getter. Facts, experiences, entities and sources already
// had one; these are the three that did not.

// GetSkill returns one skill by id.
func (s *Store) GetSkill(ctx context.Context, id uuid.UUID) (*anamnesia.Skill, error) {
	row := s.Pool.QueryRow(ctx, skillSelect+" FROM skills WHERE id = $1", id)
	return scanSkill(row)
}

// GetEdge returns one edge by id.
func (s *Store) GetEdge(ctx context.Context, id uuid.UUID) (*anamnesia.Edge, error) {
	row := s.Pool.QueryRow(ctx,
		strings.ReplaceAll(edgeSelect, "e.", "")+" FROM edges WHERE id = $1", id)
	e, err := scanEdge(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// GetWorking returns one working-memory entry by id.
func (s *Store) GetWorking(ctx context.Context, id uuid.UUID) (*anamnesia.WorkingEntry, error) {
	row := s.Pool.QueryRow(ctx, workingSelect+" FROM working_memory WHERE id = $1", id)
	w, err := scanWorking(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}
