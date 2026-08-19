package main

import (
	"strings"
	"testing"
)

// The gold set is the one thing in this feature that cannot be checked by
// running it: a mislabelled query silently reports a worse number forever.
func TestShippedCorpusIsValid(t *testing.T) {
	sources, queries, err := loadCorpus()
	if err != nil {
		t.Fatalf("loadCorpus: %v", err)
	}
	if len(sources) == 0 || len(queries) == 0 {
		t.Fatalf("corpus is empty: %d sources, %d queries", len(sources), len(queries))
	}

	ids := make(map[string]bool, len(sources))
	for _, s := range sources {
		if s.ID == "" || s.Content == "" {
			t.Errorf("source %+v has no id or no content", s)
		}
		if ids[s.ID] {
			t.Errorf("duplicate source id %q", s.ID)
		}
		ids[s.ID] = true
		if s.OccurredAt.IsZero() {
			t.Errorf("source %s has no occurred_at; decay reads it", s.ID)
		}
	}
	seen := make(map[string]bool, len(queries))
	for _, q := range queries {
		if seen[q.ID] {
			t.Errorf("duplicate query id %q", q.ID)
		}
		seen[q.ID] = true
		if strings.TrimSpace(q.Text) == "" {
			t.Errorf("query %s has no text", q.ID)
		}
		if len(q.Relevant) == 0 {
			t.Errorf("query %s labels nothing relevant, so it can only ever score zero", q.ID)
		}
		for _, r := range q.Relevant {
			if !ids[r] {
				t.Errorf("query %s says %q is relevant, but no such source exists", q.ID, r)
			}
		}
	}
}

func TestCorpusRejectsAnUnknownLabel(t *testing.T) {
	_, _, err := parseCorpus(
		[]byte(`{"id":"src-1","kind":"chat-turn","occurred_at":"2026-03-02T09:14:00Z","content":"hello there"}`),
		[]byte(`{"id":"q-1","text":"anything","relevant":["src-missing"]}`),
	)
	if err == nil {
		t.Fatal("a query labelling a nonexistent source was accepted")
	}
	if !strings.Contains(err.Error(), "src-missing") {
		t.Errorf("error %q does not name the offending id", err)
	}
}

func TestCorpusRejectsAnEmptyRelevantSet(t *testing.T) {
	_, _, err := parseCorpus(
		[]byte(`{"id":"src-1","kind":"chat-turn","occurred_at":"2026-03-02T09:14:00Z","content":"hello there"}`),
		[]byte(`{"id":"q-1","text":"anything","relevant":[]}`),
	)
	if err == nil {
		t.Fatal("a query with no relevant sources was accepted")
	}
}
