// directory.go serves the users and projects memory is filed under.
//
// This is the screen that answers "where is memory accumulating": one
// row per project with its counts and when it was last touched, which is
// also how an abandoned project makes itself visible.
package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
)

type projectView struct {
	ID           string             `json:"id"`
	Slug         string             `json:"slug"`
	User         string             `json:"user"`
	CreatedAt    string             `json:"created_at"`
	LastActivity *string            `json:"last_activity"`
	Counts       store.DomainCounts `json:"counts"`
}

type userView struct {
	ID           string             `json:"id"`
	Handle       string             `json:"handle"`
	CreatedAt    string             `json:"created_at"`
	LastActivity *string            `json:"last_activity"`
	Projects     int                `json:"projects"`
	Counts       store.DomainCounts `json:"counts"`
}

func (d Deps) handleProjects(w http.ResponseWriter, r *http.Request) {
	if d.Store == nil {
		http.Error(w, "no database configured", http.StatusServiceUnavailable)
		return
	}
	// ?user= narrows to one user, and an unknown handle is a 404 rather
	// than an empty list: "this user has no projects" and "there is no
	// such user" are different answers.
	var userID *uuid.UUID
	if handle := r.URL.Query().Get("user"); handle != "" {
		id, found, err := d.Store.LookupUser(r.Context(), handle)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, "no such user: "+handle, http.StatusNotFound)
			return
		}
		userID = &id
	}
	projects, err := d.Store.ListProjects(r.Context(), userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	items := make([]projectView, 0, len(projects))
	for _, p := range projects {
		items = append(items, projectView{
			ID:           p.ID.String(),
			Slug:         p.Slug,
			User:         p.User,
			CreatedAt:    rfc3339(p.CreatedAt),
			LastActivity: optionalTime(p.LastActivity),
			Counts:       p.Counts,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (d Deps) handleUsers(w http.ResponseWriter, r *http.Request) {
	if d.Store == nil {
		http.Error(w, "no database configured", http.StatusServiceUnavailable)
		return
	}
	users, err := d.Store.ListUsers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	items := make([]userView, 0, len(users))
	for _, u := range users {
		items = append(items, userView{
			ID:           u.ID.String(),
			Handle:       u.Handle,
			CreatedAt:    rfc3339(u.CreatedAt),
			LastActivity: optionalTime(u.LastActivity),
			Projects:     u.Projects,
			Counts:       u.Counts,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// optionalTime renders a timestamp that may not exist. A project nothing
// has been written to has no last activity, and null says that where a
// zero time would claim 1 January year one.
func optionalTime(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := rfc3339(*t)
	return &s
}
