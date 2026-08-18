// stats.go answers "what does it know", in counts.
//
// Scope here is resolved read-only. The write path's resolveScope calls
// EnsureUser and EnsureProject, so reusing it would let ?user=typo
// create a user: an endpoint that promises to change nothing has to mean
// it, and an unknown handle is a 404.
package httpapi

import (
	"net/http"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// readScope is a resolved scope plus the names it was addressed by.
type readScope struct {
	Scope   anamnesia.Scope
	User    string
	Project string
}

type scopeView struct {
	User    string  `json:"user"`
	Project *string `json:"project"`
}

func (rs readScope) view() scopeView {
	v := scopeView{User: rs.User}
	if rs.Project != "" {
		p := rs.Project
		v.Project = &p
	}
	return v
}

// resolveReadScope resolves ?user= and ?project= without creating
// anything, writing the response itself when it cannot.
func (d Deps) resolveReadScope(w http.ResponseWriter, r *http.Request) (readScope, bool) {
	if d.Store == nil {
		http.Error(w, "no database configured", http.StatusServiceUnavailable)
		return readScope{}, false
	}
	user := d.userName(r.URL.Query().Get("user"))
	uid, found, err := d.Store.LookupUser(r.Context(), user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return readScope{}, false
	}
	if !found {
		http.Error(w, "no such user: "+user, http.StatusNotFound)
		return readScope{}, false
	}
	rs := readScope{Scope: anamnesia.Scope{UserID: uid}, User: user}

	project := d.projectName(r.URL.Query().Get("project"))
	if project == "" {
		return rs, true
	}
	pid, found, err := d.Store.LookupProject(r.Context(), uid, project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return readScope{}, false
	}
	if !found {
		http.Error(w, "no such project: "+project, http.StatusNotFound)
		return readScope{}, false
	}
	rs.Scope.ProjectID = &pid
	rs.Project = project
	return rs, true
}

type statsTotals struct {
	Users         int `json:"users"`
	Projects      int `json:"projects"`
	Facts         int `json:"facts"`
	Experiences   int `json:"experiences"`
	Skills        int `json:"skills"`
	WorkingMemory int `json:"working_memory"`
	Entities      int `json:"entities"`
	Edges         int `json:"edges"`
	Sources       int `json:"sources"`
	Commitments   int `json:"commitments"`
}

type statsResponse struct {
	Scope                    scopeView                 `json:"scope"`
	Totals                   statsTotals               `json:"totals"`
	SourcesByState           map[string]int            `json:"sources_by_state"`
	Queues                   QueuePendingResponse      `json:"queues"`
	ExperiencesByAbstraction map[int]int               `json:"experiences_by_abstraction"`
	EmbeddingCoverage        map[string]store.Coverage `json:"embedding_coverage"`
}

func (d Deps) handleStats(w http.ResponseWriter, r *http.Request) {
	rs, ok := d.resolveReadScope(w, r)
	if !ok {
		return
	}
	stats, err := d.Store.Stats(r.Context(), rs.Scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, statsResponse{
		Scope: rs.view(),
		Totals: statsTotals{
			Users:         stats.Users,
			Projects:      stats.Projects,
			Facts:         stats.Facts,
			Experiences:   stats.Experiences,
			Skills:        stats.Skills,
			WorkingMemory: stats.WorkingMemory,
			Entities:      stats.Entities,
			Edges:         stats.Edges,
			Sources:       stats.Sources,
			Commitments:   stats.Commitments,
		},
		SourcesByState: stats.SourcesByState,
		Queues: QueuePendingResponse{
			ExtractPending: stats.ExtractPending,
			EmbedPending:   stats.EmbedPending,
		},
		ExperiencesByAbstraction: stats.ExperiencesByAbstraction,
		EmbeddingCoverage:        stats.EmbeddingCoverage,
	})
}
