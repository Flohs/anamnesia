// eval_corpus.go — the committed fixture corpus for `anamnesia eval`.
//
// Embedded rather than read from disk so the command works from any
// directory, and committed rather than generated so a change to the gold
// set shows up in a diff and can be argued with.
package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"
)

//go:embed testdata/eval/corpus.jsonl
var corpusJSONL []byte

//go:embed testdata/eval/queries.jsonl
var queriesJSONL []byte

type evalSource struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	OccurredAt time.Time `json:"occurred_at"`
	Content    string    `json:"content"`
}

type evalQuery struct {
	ID       string   `json:"id"`
	Text     string   `json:"text"`
	Relevant []string `json:"relevant"`
	Note     string   `json:"note,omitempty"`
}

// loadCorpus parses the embedded fixture set.
func loadCorpus() ([]evalSource, []evalQuery, error) {
	return parseCorpus(corpusJSONL, queriesJSONL)
}

// parseCorpus reads both files and cross-validates them. Split out so the
// validation itself is testable against inputs that are meant to fail.
func parseCorpus(corpus, queries []byte) ([]evalSource, []evalQuery, error) {
	var sources []evalSource
	if err := eachLine(corpus, func(n int, line []byte) error {
		var s evalSource
		if err := json.Unmarshal(line, &s); err != nil {
			return fmt.Errorf("corpus line %d: %w", n, err)
		}
		if s.ID == "" {
			return fmt.Errorf("corpus line %d: no id", n)
		}
		if s.Kind == "" {
			s.Kind = "chat-turn"
		}
		sources = append(sources, s)
		return nil
	}); err != nil {
		return nil, nil, err
	}

	known := make(map[string]bool, len(sources))
	for _, s := range sources {
		if known[s.ID] {
			return nil, nil, fmt.Errorf("corpus: duplicate source id %q", s.ID)
		}
		known[s.ID] = true
	}

	var qs []evalQuery
	if err := eachLine(queries, func(n int, line []byte) error {
		var q evalQuery
		if err := json.Unmarshal(line, &q); err != nil {
			return fmt.Errorf("queries line %d: %w", n, err)
		}
		if len(q.Relevant) == 0 {
			return fmt.Errorf("queries line %d (%s): no relevant sources, so it can only score zero", n, q.ID)
		}
		for _, r := range q.Relevant {
			if !known[r] {
				return fmt.Errorf("queries line %d (%s): relevant source %q does not exist in the corpus", n, q.ID, r)
			}
		}
		qs = append(qs, q)
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return sources, qs, nil
}

// eachLine calls fn for every non-blank line, numbering from 1.
func eachLine(b []byte, fn func(n int, line []byte) error) error {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := fn(n, line); err != nil {
			return err
		}
	}
	return sc.Err()
}
