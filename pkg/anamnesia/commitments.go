package anamnesia

import (
	"time"

	"github.com/google/uuid"
)

// CommitmentStatus is the lifecycle of an open-loop item.
type CommitmentStatus string

const (
	CommitmentOpen    CommitmentStatus = "open"
	CommitmentDone    CommitmentStatus = "done"
	CommitmentDropped CommitmentStatus = "dropped"
)

func (s CommitmentStatus) Valid() bool {
	switch s {
	case CommitmentOpen, CommitmentDone, CommitmentDropped:
		return true
	}
	return false
}

// Commitment is a tracked obligation owed by one party to another. The
// substrate keeps a single ledger so multiple dock-on agents share one
// view of what's outstanding. Owner/Beneficiary are free strings
// ("user" or a person's name) — not entity FKs — to stay cheap to write.
type Commitment struct {
	ID          uuid.UUID        `json:"id"`
	Scope       Scope            `json:"scope"`
	Owner       string           `json:"owner"`
	Beneficiary string           `json:"beneficiary"`
	Body        string           `json:"body"`
	DueAt       *time.Time       `json:"due_at,omitempty"`
	Status      CommitmentStatus `json:"status"`
	SourceID    *uuid.UUID       `json:"source_id,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}
