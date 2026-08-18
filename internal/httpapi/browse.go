// browse.go serves the memory tables themselves: one list route and one
// detail route per domain.
//
// The types on the wire are the ones in pkg/anamnesia, unchanged. They
// already tag their embeddings `json:"-"`, so a 1536-float vector never
// reaches a browser by accident, and the UI reads the same shapes the
// MCP tools return.
package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// browseDomains is every domain with a list and a detail route. Keeping
// them in one list is what stops a domain from being added to one and
// forgotten in the other.
var browseDomains = []string{
	"facts", "experiences", "skills", "entities", "edges", "sources", "working",
}

type listResponse struct {
	Items      any     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func (d Deps) browseHandler(domain string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs, ok := d.resolveReadScope(w, r)
		if !ok {
			return
		}
		b, err := parseBrowse(r, rs.Scope)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var (
			items any
			next  string
		)
		ctx := r.Context()
		switch domain {
		case "facts":
			items, next, err = d.Store.BrowseFacts(ctx, b)
		case "experiences":
			items, next, err = d.Store.BrowseExperiences(ctx, b)
		case "skills":
			items, next, err = d.Store.BrowseSkills(ctx, b)
		case "entities":
			items, next, err = d.Store.BrowseEntities(ctx, b)
		case "edges":
			items, next, err = d.Store.BrowseEdges(ctx, b)
		case "sources":
			items, next, err = d.Store.BrowseSources(ctx, b)
		case "working":
			items, next, err = d.Store.BrowseWorking(ctx, b)
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			// A cursor the caller made up is their mistake, not the
			// server's, and saying so saves a confusing 500.
			if errors.Is(err, store.ErrBadCursor) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp := listResponse{Items: items}
		if next != "" {
			resp.NextCursor = &next
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

func (d Deps) detailHandler(domain string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.Store == nil {
			http.Error(w, "no database configured", http.StatusServiceUnavailable)
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "id must be a uuid", http.StatusBadRequest)
			return
		}
		var row any
		ctx := r.Context()
		switch domain {
		case "facts":
			row, err = d.Store.GetFact(ctx, id)
		case "experiences":
			row, err = d.Store.GetExperience(ctx, id)
		case "skills":
			row, err = d.Store.GetSkill(ctx, id)
		case "entities":
			row, err = d.Store.GetEntity(ctx, id)
		case "edges":
			row, err = d.Store.GetEdge(ctx, id)
		case "sources":
			row, err = d.Store.GetSource(ctx, id)
		case "working":
			row, err = d.Store.GetWorking(ctx, id)
		default:
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, row)
	})
}

// parseBrowse reads the query string into a store filter. Unknown
// parameters are ignored; malformed ones are errors, because silently
// dropping a filter shows the caller more rows than they asked for and
// nothing says so.
func parseBrowse(r *http.Request, scope anamnesia.Scope) (store.Browse, error) {
	query := r.URL.Query()
	b := store.Browse{
		Scope:  scope,
		Q:      query.Get("q"),
		Cursor: query.Get("cursor"),
		// facts
		FactScope: query.Get("scope"),
		// experiences, entities, edges
		Kind:  query.Get("kind"),
		Topic: query.Get("topic"),
		// sources
		State: query.Get("state"),
	}
	if v := query.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return b, errors.New("limit must be a positive number")
		}
		b.Limit = n
	}
	if v := query.Get("include_deleted"); v != "" {
		deleted, err := strconv.ParseBool(v)
		if err != nil {
			return b, errors.New("include_deleted must be true or false")
		}
		b.IncludeDeleted = deleted
	}
	if v := query.Get("abstraction"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return b, errors.New("abstraction must be a number")
		}
		b.Abstraction = &n
	}
	for _, p := range []struct {
		name string
		into **uuid.UUID
	}{
		{"parent_id", &b.ParentID},
		{"from_id", &b.FromID},
		{"to_id", &b.ToID},
		{"session_id", &b.SessionID},
	} {
		v := query.Get(p.name)
		if v == "" {
			continue
		}
		id, err := uuid.Parse(v)
		if err != nil {
			return b, errors.New(p.name + " must be a uuid")
		}
		*p.into = &id
	}
	for _, p := range []struct {
		name string
		into **time.Time
	}{
		{"since", &b.Since},
		{"until", &b.Until},
	} {
		v := query.Get(p.name)
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return b, errors.New(p.name + " must be an RFC3339 timestamp")
		}
		*p.into = &t
	}
	return b, nil
}
