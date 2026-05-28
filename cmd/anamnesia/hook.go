// hook.go implements the four Claude Code hooks. Each subcommand reads
// the JSON payload from stdin (Claude Code's hook protocol), forwards it
// to the local Anamnesia server, and prints a Claude-friendly response
// to stdout. Failures are non-fatal — we never want hook errors to
// derail the user's session — so transient HTTP errors swallow.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var hookCmd = &cobra.Command{
	Use:   "hook [event]",
	Short: "Run a Claude Code hook (session-start | retrieve | session-end | pre-compact)",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runHook,
}

// hookEvent is the union payload sent to every hook endpoint on the server.
type hookEvent struct {
	SessionID      string `json:"session_id,omitempty"`
	CWD            string `json:"cwd,omitempty"`
	Project        string `json:"project,omitempty"`
	User           string `json:"user,omitempty"`
	Prompt         string `json:"prompt,omitempty"`
	MaxFacts       int    `json:"max_facts,omitempty"`
	MaxExperiences int    `json:"max_experiences,omitempty"`
}

// claudeHookInput is the schema Claude Code writes to stdin.
// We only need a handful of fields; the rest is ignored.
type claudeHookInput struct {
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd"`
	Prompt         string `json:"prompt"`
	StopReason     string `json:"stop_reason"`
	TranscriptPath string `json:"transcript_path"`
}

func runHook(cmd *cobra.Command, args []string) error {
	verb := args[0]
	cfg, err := resolveHostConfig()
	if err != nil {
		return err
	}
	input := readHookStdin()
	ev := hookEvent{
		SessionID: input.SessionID,
		CWD:       input.CWD,
		Project:   cfg.Project,
		User:      cfg.User,
		Prompt:    input.Prompt,
	}

	switch verb {
	case "session-start":
		return doSessionStart(cmd.OutOrStdout(), cfg, ev)
	case "retrieve":
		return doRetrieve(cmd.OutOrStdout(), cfg, ev)
	case "session-end":
		return doSessionCheckpoint(cfg, input, "claude-session")
	case "pre-compact":
		return doSessionCheckpoint(cfg, input, "claude-precompact")
	default:
		return fmt.Errorf("unknown hook verb %q (want session-start|retrieve|session-end|pre-compact)", verb)
	}
}

func readHookStdin() claudeHookInput {
	var in claudeHookInput
	st, _ := os.Stdin.Stat()
	if (st.Mode() & os.ModeCharDevice) != 0 {
		return in
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil || len(raw) == 0 {
		return in
	}
	_ = json.Unmarshal(raw, &in)
	return in
}

func httpPost(ctx context.Context, cfg *hostConfig, path string, body any, dst any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		cfg.ServerURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.ServerToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.ServerToken)
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	rb, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return fmt.Errorf("server %s: %s: %s", path, res.Status, string(rb))
	}
	if dst != nil && len(rb) > 0 {
		return json.Unmarshal(rb, dst)
	}
	return nil
}

// ─── session-start ───────────────────────────────────────────────────

type sessionStartResp struct {
	Facts        []factMin       `json:"facts"`
	Experiences  []experienceMin `json:"experiences"`
	PersonaBlock string          `json:"persona_block,omitempty"`
	Hint         string          `json:"hint,omitempty"`
}

type factMin struct {
	Key      string         `json:"key"`
	Value    map[string]any `json:"value"`
	FactKind string         `json:"fact_scope"`
}

type experienceMin struct {
	Title   string `json:"title,omitempty"`
	Body    string `json:"body"`
	Outcome string `json:"outcome,omitempty"`
}

func doSessionStart(w io.Writer, cfg *hostConfig, ev hookEvent) error {
	ev.MaxFacts = 50
	ev.MaxExperiences = 10
	var resp sessionStartResp
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpPost(ctx, cfg, "/v1/sessions/start", ev, &resp); err != nil {
		// Non-fatal.
		return nil
	}
	hasPersona := strings.TrimSpace(resp.PersonaBlock) != ""
	hasMemory := len(resp.Facts) > 0 || len(resp.Experiences) > 0
	if !hasPersona && !hasMemory {
		return nil
	}
	if hasPersona {
		fmt.Fprintln(w, "## How to respond")
		fmt.Fprintln(w, resp.PersonaBlock)
		fmt.Fprintln(w)
	}
	if !hasMemory {
		return nil
	}
	fmt.Fprintln(w, "## Anamnesia memory")
	if len(resp.Facts) > 0 {
		fmt.Fprintln(w, "\n**Facts**")
		for _, f := range resp.Facts {
			val, _ := json.Marshal(f.Value)
			fmt.Fprintf(w, "- `%s` (%s): %s\n", f.Key, f.FactKind, string(val))
		}
	}
	if len(resp.Experiences) > 0 {
		fmt.Fprintln(w, "\n**Recent experiences**")
		for _, e := range resp.Experiences {
			title := e.Title
			if title == "" {
				title = trimLine(e.Body, 80)
			}
			fmt.Fprintf(w, "- %s\n", title)
		}
	}
	return nil
}

// ─── retrieve ────────────────────────────────────────────────────────

type retrieveResp struct {
	Hits         []retrieveHit      `json:"hits"`
	CrossProject []crossProjectHit  `json:"cross_project,omitempty"`
}

type crossProjectHit struct {
	Domain  string `json:"domain"`
	Title   string `json:"title"`
	Project string `json:"project"`
	Recency string `json:"recency,omitempty"`
}

type retrieveHit struct {
	Domain     string          `json:"domain"`
	Score      float64         `json:"score"`
	Fact       *factMin        `json:"fact,omitempty"`
	Experience *experienceMin  `json:"experience,omitempty"`
	Skill      *skillMin       `json:"skill,omitempty"`
}

type skillMin struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func doRetrieve(w io.Writer, cfg *hostConfig, ev hookEvent) error {
	if strings.TrimSpace(ev.Prompt) == "" {
		return nil
	}
	// Pure retrieve — per-turn ingest moved to the Stop / PreCompact
	// checkpoint hooks. In-session memory is the LLM's own context.

	var resp retrieveResp
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpPost(ctx, cfg, "/v1/retrieve", ev, &resp); err != nil {
		return nil
	}
	if len(resp.Hits) == 0 && len(resp.CrossProject) == 0 {
		return nil
	}
	if len(resp.Hits) > 0 {
		fmt.Fprintln(w, "## Anamnesia retrieval")
		for _, h := range resp.Hits {
			switch h.Domain {
			case "fact":
				if h.Fact != nil {
					val, _ := json.Marshal(h.Fact.Value)
					fmt.Fprintf(w, "- fact `%s`: %s\n", h.Fact.Key, string(val))
				}
			case "experience":
				if h.Experience != nil {
					title := h.Experience.Title
					if title == "" {
						title = trimLine(h.Experience.Body, 80)
					}
					fmt.Fprintf(w, "- experience: %s\n", title)
				}
			case "skill":
				if h.Skill != nil {
					fmt.Fprintf(w, "- skill `%s`: %s\n", h.Skill.Name, h.Skill.Description)
				}
			}
		}
	}
	if len(resp.CrossProject) > 0 {
		fmt.Fprintln(w, "\n## Also from your other projects")
		for _, c := range resp.CrossProject {
			label := c.Project
			if c.Recency != "" {
				label += " · " + c.Recency
			}
			fmt.Fprintf(w, "- %s [%s]\n", c.Title, label)
		}
	}
	return nil
}

// ─── checkpoint (session-end + pre-compact) ──────────────────────────
//
// At session end and just before a context compaction, Claude Code
// passes the path to a JSONL transcript file on stdin. We read the
// chat-only turns out of it and fire-and-forget POST them to
// /v1/ingest as one source. The extract worker handles the rest in
// the background.

func doSessionCheckpoint(cfg *hostConfig, input claudeHookInput, kind string) error {
	content, err := readTranscript(input.TranscriptPath)
	if err != nil || strings.TrimSpace(content) == "" {
		// Non-fatal: missing transcript path or unreadable file just
		// means there's nothing to ingest.
		return nil
	}
	title := input.SessionID
	if title == "" {
		title = kind
	}
	fireAndForgetIngest(cfg, ingestPayload{
		Kind:    kind,
		Title:   title,
		Content: content,
		Metadata: map[string]any{
			"session_id":  input.SessionID,
			"cwd":         input.CWD,
			"stop_reason": input.StopReason,
		},
	})
	return nil
}

// readTranscript parses Claude Code's JSONL transcript file at path
// and returns a plain "role: text\n..." rendering of the chat turns.
// Tool calls and tool results are skipped — the assistant's natural
// language responses already capture what the tools surfaced. Returns
// an empty string if the file is empty, malformed, or contains no
// chat content.
func readTranscript(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec transcriptRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		text := rec.text()
		if text == "" {
			continue
		}
		role := rec.role()
		if role == "" {
			continue
		}
		sb.WriteString(role)
		sb.WriteString(": ")
		sb.WriteString(text)
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String()), nil
}

// transcriptRecord matches the loose schema Claude Code writes to the
// transcript JSONL. Each line is either a wrapped Anthropic
// `message` block (role + content[]) or a top-level `type` indicator.
// We only care about user / assistant text chunks.
type transcriptRecord struct {
	Type    string `json:"type,omitempty"`
	Message *struct {
		Role    string            `json:"role,omitempty"`
		Content json.RawMessage   `json:"content,omitempty"`
	} `json:"message,omitempty"`
}

func (r transcriptRecord) role() string {
	if r.Message != nil && r.Message.Role != "" {
		return r.Message.Role
	}
	switch r.Type {
	case "user", "assistant":
		return r.Type
	}
	return ""
}

func (r transcriptRecord) text() string {
	if r.Message == nil || len(r.Message.Content) == 0 {
		return ""
	}
	// Content is either a string ("hello") or an array of typed blocks
	// ([{type:"text", text:"..."}]). Strip everything except text blocks.
	var s string
	if err := json.Unmarshal(r.Message.Content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(r.Message.Content, &blocks); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// ingestPayload is the host-side mirror of httpapi.IngestRequest. Kept
// small here so the hook binary doesn't depend on the server package.
type ingestPayload struct {
	Kind         string         `json:"kind"`
	Title        string         `json:"title,omitempty"`
	Content      string         `json:"content"`
	Participants []string       `json:"participants,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	User         string         `json:"user,omitempty"`
	Project      string         `json:"project,omitempty"`
}

// fireAndForgetIngest pushes content into /v1/ingest with a short
// deadline. Errors are swallowed — the user's session never blocks on
// memory work.
func fireAndForgetIngest(cfg *hostConfig, p ingestPayload) {
	if strings.TrimSpace(p.Content) == "" {
		return
	}
	p.User = cfg.User
	p.Project = cfg.Project
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpPost(ctx, cfg, "/v1/ingest", p, nil)
}

func trimLine(s string, n int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
