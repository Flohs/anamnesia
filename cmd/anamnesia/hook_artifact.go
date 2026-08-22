// hook_artifact.go is the PostToolUse hook for the Artifact tool.
//
// Publishing produces a URL and nothing else keeps it. The URL is in the
// transcript, so it was never lost, but nothing went back for it: on one
// install, 31 artifacts across 9 projects existed and memory held none.
//
// Capture is deterministic on purpose. Everything else a session produces
// reaches memory through the extractor, which applies a surprise gate and
// defaults to NOOP because most of what passes through a session is
// noise. A URL is the case that gate is wrong for. There is nothing to
// judge, and an identifier that is only probably remembered is not an
// identifier, so this reads the response, reads the file, and writes the
// row. No source, no gate, no model.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flohs/anamnesia/internal/artifacts"
)

// artifactBodyMax caps the stored text extract. The embedding model reads
// only the first few thousand tokens, so beyond this the bytes cost
// storage and buy no recall.
const artifactBodyMax = 16 << 10

// artifactToolInput is the part of the Artifact tool's input worth
// keeping. The tool's own description is the best one-line label there
// is: it was written for this page, at the moment it was published.
type artifactToolInput struct {
	FilePath    string `json:"file_path"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Favicon     string `json:"favicon"`
}

// artifactRequest is the body posted to /v1/artifacts.
type artifactRequest struct {
	User         string         `json:"user,omitempty"`
	Project      string         `json:"project,omitempty"`
	ArtifactUUID string         `json:"artifact_uuid"`
	URL          string         `json:"url"`
	Title        string         `json:"title,omitempty"`
	Description  string         `json:"description,omitempty"`
	FilePath     string         `json:"file_path,omitempty"`
	Body         string         `json:"body,omitempty"`
	Meta         map[string]any `json:"meta,omitempty"`
	OccurredAt   time.Time      `json:"occurred_at,omitempty"`
}

type artifactResponse struct {
	ArtifactID string `json:"artifact_id"`
	URL        string `json:"url"`
}

func doArtifact(ctx context.Context, hc *hostConfig, input claudeHookInput) (string, error) {
	// The Artifact tool also serves `action: "list"`, whose response
	// names artifacts and their URLs without publishing anything.
	// Recording those would file every artifact the user owns as if it
	// had just been made, so the response shape is the gate.
	pub, ok := artifacts.ParsePublish(toolResponseText(input.ToolResponse))
	if !ok {
		return "not a publish, nothing recorded", nil
	}

	var in artifactToolInput
	if len(input.ToolInput) > 0 {
		_ = json.Unmarshal(input.ToolInput, &in)
	}
	path := in.FilePath
	if path == "" {
		path = pub.FilePath
	}

	title, body := in.Title, ""
	if t, b, err := readArtifactFile(path); err == nil {
		body = b
		if title == "" {
			title = t
		}
	}

	req := artifactRequest{
		User:         hc.User(),
		Project:      hc.Project(),
		ArtifactUUID: pub.UUID.String(),
		URL:          pub.URL,
		Title:        title,
		Description:  in.Description,
		FilePath:     path,
		Body:         body,
		OccurredAt:   time.Now().UTC(),
		Meta:         artifactMeta(input, in),
	}
	var resp artifactResponse
	if err := httpPost(ctx, hc, "/v1/artifacts", req, &resp); err != nil {
		return "", err
	}
	if body == "" {
		return fmt.Sprintf("recorded %s (no readable body)", pub.UUID), nil
	}
	return fmt.Sprintf("recorded %s (%d bytes of text)", pub.UUID, len(body)), nil
}

// artifactMeta records where the publish came from. agent_type is present
// when a subagent published it: tool hooks fire inside subagents too, so
// an artifact made by an agent is captured like any other, and this is
// what says so afterwards.
func artifactMeta(input claudeHookInput, in artifactToolInput) map[string]any {
	m := map[string]any{}
	if input.SessionID != "" {
		m["session_id"] = input.SessionID
	}
	if input.CWD != "" {
		m["cwd"] = input.CWD
	}
	if input.AgentType != "" {
		m["agent_type"] = input.AgentType
	}
	if in.Favicon != "" {
		m["favicon"] = in.Favicon
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// readArtifactFile extracts the title and readable text of a published
// file. The file still exists at this moment, which is the whole reason
// the live hook can do what the backfill cannot: a scratchpad is
// session-scoped, so by the time a transcript is read again it is gone.
func readArtifactFile(path string) (title, body string, err error) {
	if path == "" {
		return "", "", os.ErrNotExist
	}
	f, err := os.Open(path) //nolint:gosec // a path Claude Code just published
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	if strings.EqualFold(filepath.Ext(path), ".md") {
		body, err = artifacts.FromMarkdown(f, artifactBodyMax)
		return "", body, err
	}
	return artifacts.FromHTML(f, artifactBodyMax)
}

// toolResponseText renders a tool response that may be a bare string or a
// structured block, which is how Claude Code encodes different tools.
func toolResponseText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
