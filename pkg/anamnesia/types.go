// Package anamnesia defines the public domain types for the Anamnesia
// memory system. Single-tenant edition: each row is keyed by a free-form
// user handle and an optional project slug, so a small team can share one
// install without the SaaS notion of "tenants".
package anamnesia

import (
	"time"

	"github.com/google/uuid"
)

// Scope identifies the owner of a record. UserID is required; ProjectID
// is optional (nil means "lives at the user level, visible across all
// projects"). Slugs are resolved to UUIDs by store.EnsureUser /
// store.EnsureProject; callers normally pass the resolved Scope down.
type Scope struct {
	UserID    uuid.UUID  `json:"user_id"`
	ProjectID *uuid.UUID `json:"project_id,omitempty"`
}

// FactScope distinguishes facts that live at the user, project, or
// environment level. Identity for a fact is (Scope, FactScope, Key).
type FactScope string

const (
	FactScopeUser    FactScope = "user"
	FactScopeProject FactScope = "project"
	FactScopeEnv     FactScope = "environment"
)

func (s FactScope) Valid() bool {
	switch s {
	case FactScopeUser, FactScopeProject, FactScopeEnv:
		return true
	}
	return false
}

// Fact is one keyed claim about the world. Identity is unique per
// (Scope, FactScope, Key); upserts merge values.
type Fact struct {
	ID    uuid.UUID `json:"id"`
	Scope Scope     `json:"scope"`

	Key      string         `json:"key"`
	Value    map[string]any `json:"value"`
	FactKind FactScope      `json:"fact_scope"`

	Source   string     `json:"source,omitempty"`
	SourceID *uuid.UUID `json:"source_id,omitempty"`
	Trust    float32    `json:"trust"`
	PIITags  []string   `json:"pii_tags,omitempty"`

	Embedding  []float32 `json:"-"`
	EmbedModel string    `json:"embed_model,omitempty"`

	ValidFrom     time.Time  `json:"valid_from"`
	ValidTo       *time.Time `json:"valid_to,omitempty"`
	IngestedAt    time.Time  `json:"ingested_at"`
	InvalidatedAt *time.Time `json:"invalidated_at,omitempty"`
	SupersededBy  *uuid.UUID `json:"superseded_by,omitempty"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
}

// ExperienceKind is the experiential-memory subdivision: case-based
// (trajectory + solution), strategy (workflow / insight), or hybrid.
type ExperienceKind string

const (
	ExperienceCase     ExperienceKind = "case"
	ExperienceStrategy ExperienceKind = "strategy"
	ExperienceHybrid   ExperienceKind = "hybrid"
)

func (k ExperienceKind) Valid() bool {
	switch k {
	case ExperienceCase, ExperienceStrategy, ExperienceHybrid:
		return true
	}
	return false
}

// Outcome categorises the result of an experience for replay/learning.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
	OutcomePartial Outcome = "partial"
)

// Experience is a free-text record of "what the agent did and what
// happened".
type Experience struct {
	ID    uuid.UUID `json:"id"`
	Scope Scope     `json:"scope"`

	Kind        ExperienceKind `json:"kind"`
	Abstraction int            `json:"abstraction"`
	Title       string         `json:"title,omitempty"`
	Body        string         `json:"body"`
	Outcome     Outcome        `json:"outcome,omitempty"`
	Meta        map[string]any `json:"meta,omitempty"`

	Trust      float32   `json:"trust"`
	Importance float32   `json:"importance"`
	Relevance  float32   `json:"relevance"`
	UseCount   int       `json:"use_count"`
	LastUsedAt time.Time `json:"last_used_at"`
	PIITags    []string  `json:"pii_tags,omitempty"`

	// Episode columns. occurred_at is the world-time the experience
	// describes (e.g. when the meeting happened); ingested_at is when
	// we learned about it. Participants/topic/source_id/parent_id make
	// temporal-window queries cheap. Provenance points back to the
	// source span the row was extracted from.
	OccurredAt   *time.Time     `json:"occurred_at,omitempty"`
	SourceID     *uuid.UUID     `json:"source_id,omitempty"`
	Participants []string       `json:"participants,omitempty"`
	Topic        string         `json:"topic,omitempty"`
	ParentID     *uuid.UUID     `json:"parent_id,omitempty"`
	Provenance   map[string]any `json:"provenance,omitempty"`

	Embedding  []float32 `json:"-"`
	EmbedModel string    `json:"embed_model,omitempty"`

	ValidFrom     time.Time  `json:"valid_from"`
	ValidTo       *time.Time `json:"valid_to,omitempty"`
	IngestedAt    time.Time  `json:"ingested_at"`
	InvalidatedAt *time.Time `json:"invalidated_at,omitempty"`
	SupersededBy  *uuid.UUID `json:"superseded_by,omitempty"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
}

// SkillKind classifies a registered tool.
type SkillKind string

const (
	SkillFunction SkillKind = "function"
	SkillScript   SkillKind = "script"
	SkillAPI      SkillKind = "api"
	SkillMCP      SkillKind = "mcp"
)

func (k SkillKind) Valid() bool {
	switch k {
	case SkillFunction, SkillScript, SkillAPI, SkillMCP:
		return true
	}
	return false
}

// Skill is a registered callable. Identity is (Scope, Name).
type Skill struct {
	ID    uuid.UUID `json:"id"`
	Scope Scope     `json:"scope"`

	Name        string         `json:"name"`
	Kind        SkillKind      `json:"kind"`
	Description string         `json:"description,omitempty"`
	Signature   map[string]any `json:"signature,omitempty"`
	Body        string         `json:"body,omitempty"`
	Meta        map[string]any `json:"meta,omitempty"`

	UseCount   int        `json:"use_count"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	DeletedAt  *time.Time `json:"deleted_at,omitempty"`
}

// Artifact is a page Claude Code published, kept as a pointer rather than
// a copy. Identity is (Scope.UserID, ArtifactUUID): republishing a file
// redeploys to the same URL, so it updates this row instead of adding one.
//
// Body is the readable text of the page at publish time, and is what gets
// embedded. It is empty for an artifact recovered from a transcript after
// its source file was cleaned up, which is the usual case for anything
// older than the current session; the pointer is still worth having, and
// a later republish fills the body in.
type Artifact struct {
	ID    uuid.UUID `json:"id"`
	Scope Scope     `json:"scope"`

	ArtifactUUID uuid.UUID `json:"artifact_uuid"`
	URL          string    `json:"url"`
	Title        string    `json:"title,omitempty"`
	Description  string    `json:"description,omitempty"`
	FilePath     string    `json:"file_path,omitempty"`
	Body         string    `json:"body,omitempty"`

	Meta map[string]any `json:"meta,omitempty"`

	// Project is the slug the artifact was published under, filled in by
	// listings so a reader sees a name rather than a uuid. Not stored:
	// project_id is the record.
	Project string `json:"project,omitempty"`

	Embedding  []float32 `json:"-"`
	EmbedModel string    `json:"embed_model,omitempty"`

	OccurredAt time.Time  `json:"occurred_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	DeletedAt  *time.Time `json:"deleted_at,omitempty"`
}

// Label is the best short name for an artifact: its title if it declared
// one, otherwise the description it was published with.
func (a *Artifact) Label() string {
	if a.Title != "" {
		return a.Title
	}
	return a.Description
}

// WorkingRole tags an entry in a working-memory session.
type WorkingRole string

const (
	WorkingObservation WorkingRole = "observation"
	WorkingPlan        WorkingRole = "plan"
	WorkingState       WorkingRole = "state"
	WorkingToolOutput  WorkingRole = "tool_output"
)

func (r WorkingRole) Valid() bool {
	switch r {
	case WorkingObservation, WorkingPlan, WorkingState, WorkingToolOutput:
		return true
	}
	return false
}

// WorkingEntry is a single position in a session's working memory.
type WorkingEntry struct {
	ID    uuid.UUID `json:"id"`
	Scope Scope     `json:"scope"`

	SessionID uuid.UUID      `json:"session_id"`
	Position  int            `json:"position"`
	Role      WorkingRole    `json:"role"`
	Body      string         `json:"body"`
	Meta      map[string]any `json:"meta,omitempty"`

	FoldedInto *uuid.UUID `json:"folded_into,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Domain identifies which table a search hit came from.
type Domain string

const (
	DomainFact       Domain = "fact"
	DomainExperience Domain = "experience"
	DomainSkill      Domain = "skill"
	DomainWorking    Domain = "working"
	DomainArtifact   Domain = "artifact"
)

func (d Domain) Valid() bool {
	switch d {
	case DomainFact, DomainExperience, DomainSkill, DomainWorking, DomainArtifact:
		return true
	}
	return false
}

// SearchHit is a scored result from cross-domain retrieval. Exactly one
// of Fact / Experience / Skill / Artifact is populated; Domain says which.
type SearchHit struct {
	Domain     Domain      `json:"domain"`
	Fact       *Fact       `json:"fact,omitempty"`
	Experience *Experience `json:"experience,omitempty"`
	Skill      *Skill      `json:"skill,omitempty"`
	Artifact   *Artifact   `json:"artifact,omitempty"`

	Score        float64 `json:"score"`
	VectorRank   int     `json:"vector_rank,omitempty"`
	LexicalRank  int     `json:"lexical_rank,omitempty"`
	GraphRank    int     `json:"graph_rank,omitempty"`
	RerankerRank int     `json:"reranker_rank,omitempty"`
}

func (h SearchHit) ID() uuid.UUID {
	switch h.Domain {
	case DomainFact:
		if h.Fact != nil {
			return h.Fact.ID
		}
	case DomainExperience:
		if h.Experience != nil {
			return h.Experience.ID
		}
	case DomainSkill:
		if h.Skill != nil {
			return h.Skill.ID
		}
	case DomainArtifact:
		if h.Artifact != nil {
			return h.Artifact.ID
		}
	}
	return uuid.Nil
}

func (h SearchHit) Body() string {
	switch h.Domain {
	case DomainFact:
		if h.Fact != nil {
			return h.Fact.Key
		}
	case DomainExperience:
		if h.Experience != nil {
			if h.Experience.Title != "" {
				return h.Experience.Title + "\n\n" + h.Experience.Body
			}
			return h.Experience.Body
		}
	case DomainSkill:
		if h.Skill != nil {
			return h.Skill.Name + "\n\n" + h.Skill.Description
		}
	case DomainArtifact:
		if h.Artifact != nil {
			return h.Artifact.Label() + "\n\n" + h.Artifact.Body
		}
	}
	return ""
}

// Source is a raw piece of content that may yield extracted memory.
// One row per ingested artifact: a chat turn, a transcript, a document,
// a note, a tool output. The extractor reads pending sources, decides
// what's worth keeping, and writes facts / experiences. Raw content
// expires after ExpiresAt unless PreserveRaw is true.
type Source struct {
	ID              uuid.UUID      `json:"id"`
	Scope           Scope          `json:"scope"`
	Kind            string         `json:"kind"`
	ExternalRef     string         `json:"external_ref,omitempty"`
	Title           string         `json:"title,omitempty"`
	Participants    []string       `json:"participants,omitempty"`
	OccurredAt      time.Time      `json:"occurred_at"`
	IngestedAt      time.Time      `json:"ingested_at"`
	RawContent      string         `json:"raw_content,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	ExtractionState string         `json:"extraction_state"`
	ExtractedAt     *time.Time     `json:"extracted_at,omitempty"`
	ExtractionError string         `json:"extraction_error,omitempty"`
	OpsProduced     int            `json:"ops_produced"`
	PreserveRaw     bool           `json:"preserve_raw"`
	ExpiresAt       time.Time      `json:"expires_at"`
}

// Extraction states.
const (
	ExtractionPending = "pending"
	ExtractionDone    = "done"
	ExtractionFailed  = "failed"
	ExtractionSkipped = "skipped"
)

// Entity is a node in the memory graph. Identity is (Scope, Kind, Name).
type Entity struct {
	ID        uuid.UUID      `json:"id"`
	Scope     Scope          `json:"scope"`
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Props     map[string]any `json:"props,omitempty"`
	Embedding []float32      `json:"-"`
	CreatedAt time.Time      `json:"created_at"`
}

// Edge is a typed, bitemporal relation between two entities.
type Edge struct {
	ID            uuid.UUID      `json:"id"`
	From          uuid.UUID      `json:"from_id"`
	To            uuid.UUID      `json:"to_id"`
	Kind          string         `json:"kind"`
	Props         map[string]any `json:"props,omitempty"`
	ValidFrom     time.Time      `json:"valid_from"`
	ValidTo       *time.Time     `json:"valid_to,omitempty"`
	IngestedAt    time.Time      `json:"ingested_at"`
	InvalidatedAt *time.Time     `json:"invalidated_at,omitempty"`
	Source        string         `json:"source,omitempty"`
	Trust         float32        `json:"trust"`
}

// AuditEntry is a row from the audit_log.
type AuditEntry struct {
	ID        int64          `json:"id"`
	At        time.Time      `json:"at"`
	UserID    *uuid.UUID     `json:"user_id,omitempty"`
	ProjectID *uuid.UUID     `json:"project_id,omitempty"`
	Op        string         `json:"op"`
	Target    string         `json:"target"`
	TargetID  *uuid.UUID     `json:"target_id,omitempty"`
	Actor     string         `json:"actor"`
	Payload   map[string]any `json:"payload,omitempty"`
}
