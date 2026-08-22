// artifacts.go records and lists published artifacts.
//
// POST here is deliberately not /v1/ingest. Ingest lands a source row for
// the extractor, which applies a surprise gate and defaults to NOOP,
// because most of what passes through a session is noise. An artifact URL
// is the opposite case: there is nothing to judge, and a URL that is only
// probably remembered is not a URL. So this writes the row directly, with
// no source, no gate and no model.
package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// ArtifactRequest is one published artifact, as the hook reports it.
type ArtifactRequest struct {
	User    string `json:"user,omitempty"`
	Project string `json:"project,omitempty"`

	ArtifactUUID string `json:"artifact_uuid"`
	URL          string `json:"url"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
	Body         string `json:"body,omitempty"`

	Meta       map[string]any `json:"meta,omitempty"`
	OccurredAt time.Time      `json:"occurred_at,omitempty"`
}

type ArtifactResponse struct {
	ArtifactID uuid.UUID `json:"artifact_id"`
	URL        string    `json:"url"`
}

type ArtifactListResponse struct {
	Scope     scopeView             `json:"scope"`
	Artifacts []*anamnesia.Artifact `json:"artifacts"`
}

func (d Deps) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		d.recordArtifact(w, r)
	case http.MethodGet:
		d.listArtifacts(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (d Deps) recordArtifact(w http.ResponseWriter, r *http.Request) {
	var req ArtifactRequest
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(req.ArtifactUUID))
	if err != nil {
		http.Error(w, "artifact_uuid must be a uuid: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		http.Error(w, "url required", http.StatusBadRequest)
		return
	}
	scope, err := d.resolveWriteScope(r.Context(), &HookEvent{User: req.User, Project: req.Project})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a := &anamnesia.Artifact{
		Scope:        scope,
		ArtifactUUID: id,
		URL:          req.URL,
		Title:        req.Title,
		Description:  req.Description,
		FilePath:     req.FilePath,
		Body:         req.Body,
		Meta:         req.Meta,
		OccurredAt:   req.OccurredAt,
	}
	// Embed inline when there is an embedder, so an artifact is findable
	// from the moment it is published rather than after the next worker
	// tick. A failure here is not fatal: the row lands with a NULL
	// embedding, which is exactly what the backfill lane picks up.
	if d.Retrieval != nil && d.Retrieval.Embedder != nil {
		text := a.Label()
		if a.Body != "" {
			text += "\n\n" + a.Body
		}
		vecs, err := d.Retrieval.Embedder.Embed(r.Context(), []string{text})
		if err == nil && len(vecs) == 1 && len(vecs[0]) > 0 {
			a.Embedding = vecs[0]
			a.EmbedModel = d.Retrieval.Embedder.Model()
		} else if err != nil && d.Log != nil {
			d.Log.Warn("artifact: inline embed failed (will be backfilled)", "err", err)
		}
	}
	if err := d.Store.UpsertArtifact(r.Context(), a); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, ArtifactResponse{ArtifactID: a.ID, URL: a.URL})
}

func (d Deps) listArtifacts(w http.ResponseWriter, r *http.Request) {
	rs, ok := d.resolveReadScope(w, r)
	if !ok {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = n
	}
	list, err := d.Store.ListArtifacts(r.Context(), rs.Scope, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []*anamnesia.Artifact{}
	}
	writeJSON(w, http.StatusOK, ArtifactListResponse{Scope: rs.view(), Artifacts: list})
}
