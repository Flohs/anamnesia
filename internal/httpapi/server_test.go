package httpapi

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

func TestHumanRecency(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		when time.Time
		want string
	}{
		{"zero", time.Time{}, ""},
		{"today", now.Add(-2 * time.Hour), "today"},
		{"yesterday", now.Add(-30 * time.Hour), "yesterday"},
		{"three days", now.Add(-3 * 24 * time.Hour), "3 days ago"},
		{"last week", now.Add(-9 * 24 * time.Hour), "last week"},
		{"old date", now.Add(-40 * 24 * time.Hour), "2026-04-18"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := humanRecency(c.when, now); got != c.want {
				t.Errorf("humanRecency(%v) = %q, want %q", c.when, got, c.want)
			}
		})
	}
}

func TestCrossFields(t *testing.T) {
	pid := uuid.New()
	occurred := time.Date(2026, 5, 25, 9, 0, 0, 0, time.UTC)
	ingested := time.Date(2026, 5, 26, 9, 0, 0, 0, time.UTC)

	t.Run("fact uses key + ingested_at", func(t *testing.T) {
		h := anamnesia.SearchHit{
			Domain: anamnesia.DomainFact,
			Fact: &anamnesia.Fact{
				Scope: anamnesia.Scope{ProjectID: &pid}, Key: "deploy.target", IngestedAt: ingested,
			},
		}
		gotPid, title, when, ok := crossFields(h)
		if !ok || gotPid == nil || *gotPid != pid || title != "deploy.target" || !when.Equal(ingested) {
			t.Fatalf("fact: ok=%v pid=%v title=%q when=%v", ok, gotPid, title, when)
		}
	})

	t.Run("experience prefers occurred_at + title", func(t *testing.T) {
		h := anamnesia.SearchHit{
			Domain: anamnesia.DomainExperience,
			Experience: &anamnesia.Experience{
				Scope: anamnesia.Scope{ProjectID: &pid}, Title: "debugged RRF fusion",
				Body: "long body", OccurredAt: &occurred, IngestedAt: ingested,
			},
		}
		gotPid, title, when, ok := crossFields(h)
		if !ok || title != "debugged RRF fusion" || !when.Equal(occurred) {
			t.Fatalf("exp: ok=%v title=%q when=%v (want occurred_at)", ok, title, when)
		}
		_ = gotPid
	})

	t.Run("skill is not surfaced", func(t *testing.T) {
		h := anamnesia.SearchHit{Domain: anamnesia.DomainSkill, Skill: &anamnesia.Skill{Name: "x"}}
		if _, _, _, ok := crossFields(h); ok {
			t.Fatal("skill hits should not be surfaced cross-project")
		}
	})
}
