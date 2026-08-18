package httpapi

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/pkg/anamnesia"
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

// The activity stream is served through withLogging like every other
// route. If its wrapper hides the underlying writer, the stream cannot
// flush and cannot clear the 60s write deadline, so it buffers until the
// server severs it. Both are silent failures, hence this test.
func TestLoggingWrapperStaysFlushable(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := withLogging(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		fmt.Fprintf(w, "deadline=%v flush=%v", rc.SetWriteDeadline(time.Time{}), rc.Flush())
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "deadline=<nil> flush=<nil>"; got != want {
		t.Errorf("through withLogging: %s; want %s", got, want)
	}
}
