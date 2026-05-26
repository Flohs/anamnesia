// Package pii detects and optionally redacts personally identifiable
// information before it reaches the embedding model or the database.
//
// Three implementations:
//
//	none      no-op (default; lets local dev work without a sidecar)
//	regex     in-process regex sweep over common PII categories
//	presidio  HTTP call to a Microsoft Presidio analyzer (recommended in prod)
//
// All implementations satisfy Detector. Switching is a config change.
package pii

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Detector finds PII spans in text and returns either a redacted copy
// (if Mode == ModeRedact) or the original text plus tags.
type Detector interface {
	Scrub(ctx context.Context, text string) (cleaned string, tags []string, err error)
}

// Mode controls whether matches are replaced with placeholders or merely tagged.
type Mode string

const (
	ModeTag    Mode = "tag"
	ModeRedact Mode = "redact"
)

// New returns a Detector for the named provider. provider="" or "none"
// returns a no-op detector.
func New(provider, presidioURL string, mode Mode) (Detector, error) {
	if mode == "" {
		mode = ModeTag
	}
	switch provider {
	case "", "none":
		return noop{}, nil
	case "regex":
		return &regexDetector{mode: mode}, nil
	case "presidio":
		if presidioURL == "" {
			return nil, errors.New("pii: presidio url required")
		}
		return &presidioDetector{
			url:  presidioURL,
			mode: mode,
			http: &http.Client{Timeout: 5 * time.Second},
		}, nil
	default:
		return nil, errors.New("pii: unknown provider " + provider)
	}
}

type noop struct{}

func (noop) Scrub(_ context.Context, t string) (string, []string, error) { return t, nil, nil }

// regexDetector covers the common categories. It is intentionally simple;
// the Presidio path is preferred for production.
type regexDetector struct{ mode Mode }

var (
	rxEmail = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	rxPhone = regexp.MustCompile(`\b(?:\+?\d{1,3}[ \-.])?(?:\(\d{2,4}\)[ \-.]?|\d{2,4}[ \-.])?\d{3,4}[ \-.]?\d{3,4}\b`)
	rxIBAN  = regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{4}\d{7}([A-Z0-9]?){0,16}\b`)
	rxIPv4  = regexp.MustCompile(`\b(?:25[0-5]|2[0-4]\d|[01]?\d?\d)(?:\.(?:25[0-5]|2[0-4]\d|[01]?\d?\d)){3}\b`)
)

func (r *regexDetector) Scrub(_ context.Context, text string) (string, []string, error) {
	var tags []string
	out := text

	apply := func(rx *regexp.Regexp, tag string) {
		if rx.MatchString(out) {
			tags = append(tags, tag)
			if r.mode == ModeRedact {
				out = rx.ReplaceAllString(out, "["+tag+"]")
			}
		}
	}
	apply(rxEmail, "EMAIL")
	apply(rxPhone, "PHONE")
	apply(rxIBAN, "IBAN")
	apply(rxIPv4, "IP")
	return out, tags, nil
}

// presidioDetector calls Microsoft Presidio's analyzer endpoint.
type presidioDetector struct {
	url  string
	mode Mode
	http *http.Client
}

type presidioResp struct {
	EntityType string `json:"entity_type"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
}

func (p *presidioDetector) Scrub(ctx context.Context, text string) (string, []string, error) {
	body, _ := json.Marshal(map[string]any{"text": text, "language": "en"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(p.url, "/")+"/analyze", bytes.NewReader(body))
	if err != nil {
		return text, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return text, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return text, nil, errors.New("presidio: " + resp.Status + ": " + string(raw))
	}
	var spans []presidioResp
	if err := json.Unmarshal(raw, &spans); err != nil {
		return text, nil, err
	}
	tags := make([]string, 0, len(spans))
	cleaned := text
	if p.mode == ModeRedact && len(spans) > 0 {
		// Replace from end-to-start so offsets stay valid.
		b := []byte(cleaned)
		for i := len(spans) - 1; i >= 0; i-- {
			s := spans[i]
			if s.Start < 0 || s.End > len(b) {
				continue
			}
			repl := []byte("[" + s.EntityType + "]")
			b = append(b[:s.Start], append(repl, b[s.End:]...)...)
		}
		cleaned = string(b)
	}
	for _, s := range spans {
		tags = append(tags, s.EntityType)
	}
	return cleaned, tags, nil
}
