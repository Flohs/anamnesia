// hook.go implements the Claude Code hooks. Each subcommand reads the JSON
// payload Claude Code writes to stdin, talks to the local server, and
// prints whatever Claude should see on stdout.
//
// Three rules govern everything here:
//
// A hook never breaks a session. Every failure path returns success with no
// output, so a stopped server or an unreachable database costs the user
// nothing but the memory they would have had.
//
// A hook never disappears silently either. Every run appends one line to
// ~/.anamnesia/hooks.log, which is what lets `anamnesia doctor` tell the
// difference between "working" and "failing on every turn" — previously
// indistinguishable, because both looked like an empty session.
//
// A hook never blocks for long. Deadlines are short, and reading the
// transcript is incremental: each checkpoint sends only what has been added
// since the last one.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var hookCmd = &cobra.Command{
	Use:    "hook [event]",
	Short:  "Run a Claude Code hook (session-start | retrieve | session-end | pre-compact)",
	Long:   "Invoked by Claude Code's hooks. Not meant to be run by hand.",
	Args:   cobra.MinimumNArgs(1),
	Hidden: true,
	RunE:   runHook,
}

// hookEvent is the union payload sent to the server's hook endpoints.
type hookEvent struct {
	SessionID      string `json:"session_id,omitempty"`
	CWD            string `json:"cwd,omitempty"`
	Project        string `json:"project,omitempty"`
	User           string `json:"user,omitempty"`
	Prompt         string `json:"prompt,omitempty"`
	MaxFacts       int    `json:"max_facts,omitempty"`
	MaxExperiences int    `json:"max_experiences,omitempty"`
}

// claudeHookInput is the schema Claude Code writes to stdin. Only the
// fields we use are declared; the rest is ignored.
type claudeHookInput struct {
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd"`
	Prompt         string `json:"prompt"`
	StopReason     string `json:"stop_reason"`
	TranscriptPath string `json:"transcript_path"`
	HookEventName  string `json:"hook_event_name"`
	Source         string `json:"source"`
	Trigger        string `json:"trigger"`
}

// errServerUnreachable is logged when a hook found no server to talk to.
var errServerUnreachable = errors.New("server not running, so this hook did nothing")

// hookTimeouts are the per-verb budgets. UserPromptSubmit is on the
// critical path of every prompt, so it gets the tightest one.
var hookTimeouts = map[string]time.Duration{
	"session-start": 6 * time.Second,
	"retrieve":      2500 * time.Millisecond,
	"session-end":   20 * time.Second,
	"pre-compact":   20 * time.Second,
}

func runHook(cmd *cobra.Command, args []string) error {
	verb := args[0]
	started := time.Now()

	switch verb {
	case "session-start", "retrieve", "session-end", "pre-compact":
	default:
		return fmt.Errorf("unknown hook verb %q (want session-start|retrieve|session-end|pre-compact)", verb)
	}

	hc, err := loadHostConfig()
	if err != nil {
		logHook(verb, started, err, "config unreadable")
		return nil // never break the session over a bad config
	}
	input := readHookStdin()
	ev := hookEvent{
		SessionID: input.SessionID,
		CWD:       input.CWD,
		Project:   hc.Project(),
		User:      hc.User(),
		Prompt:    input.Prompt,
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), hookTimeouts[verb])
	defer cancel()

	// SessionStart can afford to wait a moment for an auto-start, because
	// getting memory into the first prompt is the whole point. The others
	// kick the start off and move on.
	wait := time.Duration(0)
	if verb == "session-start" {
		wait = 4 * time.Second
	}
	if !ensureServerRunning(ctx, hc, wait) {
		// Recorded as a failure, not a quiet skip. The session carries on
		// regardless, but this is precisely the state doctor has to be able
		// to see: memory silently doing nothing, every single turn.
		logHook(verb, started, errServerUnreachable, "")
		return nil
	}

	var note string
	switch verb {
	case "session-start":
		note, err = doSessionStart(ctx, cmd.OutOrStdout(), hc, ev)
	case "retrieve":
		note, err = doRetrieve(ctx, cmd.OutOrStdout(), hc, ev)
	case "session-end":
		note, err = doCheckpoint(ctx, hc, input, "claude-session")
	case "pre-compact":
		note, err = doCheckpoint(ctx, hc, input, "claude-precompact")
	}
	logHook(verb, started, err, note)
	return nil
}

// readHookStdin decodes Claude Code's payload under a deadline.
//
// The deadline matters: stdin is a pipe, and if the writer never closes it
// an unguarded read blocks forever, which manifests as Claude Code hanging
// rather than as anything recognisable as a memory problem.
func readHookStdin() claudeHookInput {
	var in claudeHookInput

	info, err := os.Stdin.Stat()
	if err != nil {
		return in // no usable stdin; treat as an empty payload
	}
	if (info.Mode() & os.ModeCharDevice) != 0 {
		return in // a terminal, so nobody is piping us a payload
	}

	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
		done <- result{raw, err}
	}()

	select {
	case r := <-done:
		if r.err != nil || len(r.raw) == 0 {
			return in
		}
		_ = json.Unmarshal(r.raw, &in)
	case <-time.After(2 * time.Second):
		// Nothing arrived; carry on with an empty payload.
	}
	return in
}

// httpPost sends a JSON body and decodes a JSON reply.
//
// Any 2xx counts: /v1/ingest answers 202 and /v1/experience answers 201, so
// a 200-only check classified every successful write as a failure.
func httpPost(ctx context.Context, hc *hostConfig, path string, body, dst any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hc.ServerURL()+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := hc.Token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("server %s: %s: %s", path, res.Status, strings.TrimSpace(string(rb)))
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

func doSessionStart(ctx context.Context, w io.Writer, hc *hostConfig, ev hookEvent) (string, error) {
	ev.MaxFacts = 50
	ev.MaxExperiences = 10
	var resp sessionStartResp
	if err := httpPost(ctx, hc, "/v1/sessions/start", ev, &resp); err != nil {
		return "", err
	}
	hasPersona := strings.TrimSpace(resp.PersonaBlock) != ""
	hasMemory := len(resp.Facts) > 0 || len(resp.Experiences) > 0
	note := fmt.Sprintf("%d facts, %d experiences", len(resp.Facts), len(resp.Experiences))
	if !hasPersona && !hasMemory {
		return note, nil
	}
	if hasPersona {
		fmt.Fprintln(w, "## How to respond")
		fmt.Fprintln(w, resp.PersonaBlock)
		fmt.Fprintln(w)
	}
	if !hasMemory {
		return note, nil
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
	return note, nil
}

// ─── retrieve ────────────────────────────────────────────────────────

type retrieveResp struct {
	Hits         []retrieveHit     `json:"hits"`
	CrossProject []crossProjectHit `json:"cross_project,omitempty"`
}

type crossProjectHit struct {
	Domain  string `json:"domain"`
	Title   string `json:"title"`
	Project string `json:"project"`
	Recency string `json:"recency,omitempty"`
}

type retrieveHit struct {
	Domain     string         `json:"domain"`
	Score      float64        `json:"score"`
	Fact       *factMin       `json:"fact,omitempty"`
	Experience *experienceMin `json:"experience,omitempty"`
	Skill      *skillMin      `json:"skill,omitempty"`
}

type skillMin struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func doRetrieve(ctx context.Context, w io.Writer, hc *hostConfig, ev hookEvent) (string, error) {
	if strings.TrimSpace(ev.Prompt) == "" {
		return "empty prompt", nil
	}
	var resp retrieveResp
	if err := httpPost(ctx, hc, "/v1/retrieve", ev, &resp); err != nil {
		return "", err
	}
	note := fmt.Sprintf("%d hits, %d cross-project", len(resp.Hits), len(resp.CrossProject))
	if len(resp.Hits) == 0 && len(resp.CrossProject) == 0 {
		return note, nil
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
	return note, nil
}

// segment is one piece of a checkpoint: contiguous turns that look like one
// subject, and when the first of them happened.
type segment struct {
	Content string
	At      time.Time
	// EndOffset is the absolute byte offset in the transcript just past
	// this segment's last line. It lets a partial post advance the offset
	// to the last segment that actually landed, instead of losing progress
	// on every failure.
	EndOffset int64
}

// minSegmentBytes is the shortest tail worth posting on its own. Below it a
// segment merges backwards: a one-word coda ("ok", "thanks") is not its own
// idea, and posting it costs a gate evaluation to learn that. Kept well
// below the length of even a short but genuine exchange, so a real topic
// change that happens to be terse is never folded back into the one before
// it.
const minSegmentBytes = 40

// ─── checkpoints (session-end + pre-compact) ─────────────────────────

// doCheckpoint sends the part of the transcript that has not been sent
// before, and records how far it got.
//
// The offset is what keeps this linear. Re-reading the whole transcript at
// every checkpoint means a long session is ingested over and over, and the
// extractor pays for the same content each time.
func doCheckpoint(ctx context.Context, hc *hostConfig, input claudeHookInput, kind string) (string, error) {
	if input.TranscriptPath == "" {
		return "no transcript path", nil
	}
	offset := readOffset(input.SessionID, input.TranscriptPath)
	segs, next, err := readTranscriptFrom(input.TranscriptPath, offset,
		hc.Dur("ingest.segment_gap"), hc.Int("ingest.segment_max_bytes"))
	if err != nil {
		return "", err
	}
	if len(segs) == 0 {
		// Still record the offset: a checkpoint over tool-only turns has
		// nothing to say but has definitely consumed those bytes.
		_ = writeOffset(input.SessionID, input.TranscriptPath, next)
		return "nothing new since last checkpoint", nil
	}

	title := input.SessionID
	if title == "" {
		title = kind
	}
	sent := 0
	var lastEnd int64
	segStart := offset
	for i, seg := range segs {
		at := seg.At
		payload := ingestPayload{
			Kind:        kind,
			Title:       title,
			ExternalRef: fmt.Sprintf("%s#%d", input.SessionID, segStart),
			Content:     seg.Content,
			User:        hc.User(),
			Project:     hc.Project(),
			Metadata: map[string]any{
				"session_id":  input.SessionID,
				"cwd":         input.CWD,
				"stop_reason": input.StopReason,
				"trigger":     input.Trigger,
				"byte_range":  fmt.Sprintf("%d-%d", segStart, seg.EndOffset),
				"segment":     i,
				"segments":    len(segs),
			},
		}
		if !at.IsZero() {
			payload.OccurredAt = &at
		}
		if err := httpPost(ctx, hc, "/v1/ingest", payload, nil); err != nil {
			// Advance the offset to the last segment that actually landed,
			// so a retry re-sends only what failed rather than everything
			// again on top of a growing transcript: without this, a
			// checkpoint that runs out of time can never catch up, because
			// each retry re-reads a larger range than the one before it.
			// Re-sending is safe (the gate deduplicates); losing a segment
			// that already landed is not.
			if sent > 0 {
				if werr := writeOffset(input.SessionID, input.TranscriptPath, lastEnd); werr != nil {
					return fmt.Sprintf("ingested %d of %d segments, offset not saved", sent, len(segs)), werr
				}
			}
			return fmt.Sprintf("ingested %d of %d segments", sent, len(segs)), err
		}
		sent++
		lastEnd = seg.EndOffset
		segStart = seg.EndOffset
	}
	if err := writeOffset(input.SessionID, input.TranscriptPath, next); err != nil {
		return fmt.Sprintf("ingested %d segments, offset not saved", sent), err
	}

	note := fmt.Sprintf("ingested %d segments", sent)
	if hc.Bool("graph.extract") {
		// One extra post over the whole checkpoint, after every segment has
		// landed: a per-segment graph pass could only ever connect entities
		// within the same segment, and the segments are already cut at
		// topic boundaries. A failure here must not fail the checkpoint or
		// hold back the offset already written above: the segments are the
		// memory, this is an extra.
		contents := make([]string, len(segs))
		for i, seg := range segs {
			contents[i] = seg.Content
		}
		payload := ingestPayload{
			Kind:        "claude-session-graph",
			Title:       title,
			ExternalRef: fmt.Sprintf("%s#%d-graph", input.SessionID, offset),
			Content:     strings.Join(contents, "\n"),
			User:        hc.User(),
			Project:     hc.Project(),
		}
		if at := segs[0].At; !at.IsZero() {
			payload.OccurredAt = &at
		}
		if err := httpPost(ctx, hc, "/v1/ingest", payload, nil); err != nil {
			note += "; graph source failed: " + err.Error()
		}
	}
	return note, nil
}

// readTranscriptFrom renders the chat turns in path starting at offset,
// returning the text and the new offset.
//
// Tool calls and tool results are skipped: the assistant's own prose
// already says what the tools produced, at a fraction of the tokens.
func readTranscriptFrom(path string, offset int64, gap time.Duration, maxBytes int) ([]segment, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	// A transcript that shrank was replaced, so the old offset is
	// meaningless and we start over.
	if info.Size() < offset {
		offset = 0
	}
	if info.Size() == offset {
		return nil, offset, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, offset, err
	}

	// Only whole lines are consumed, so a checkpoint landing mid-write
	// does not corrupt the next read.
	consumed := len(raw)
	if i := bytes.LastIndexByte(raw, '\n'); i >= 0 {
		consumed = i + 1
	} else {
		return nil, offset, nil // no complete line yet
	}

	// raw[:consumed] always ends in '\n' (only whole lines are consumed
	// above), so Split always leaves one trailing empty element. Drop it
	// before using line lengths for byte accounting, or every read would
	// be off by one.
	rawLines := strings.Split(string(raw[:consumed]), "\n")
	rawLines = rawLines[:len(rawLines)-1]

	var (
		segs    []segment
		sb      strings.Builder
		segAt   time.Time
		prevAt  time.Time
		haveSeg bool
		pos     = offset
	)
	// flush closes out the current segment, ending it at end: the exact
	// byte position up to which its lines have been consumed. A failed
	// post can then resume from there rather than from the start of the
	// whole batch.
	flush := func(end int64) {
		if body := strings.TrimSpace(sb.String()); body != "" {
			segs = append(segs, segment{Content: body, At: segAt, EndOffset: end})
		}
		sb.Reset()
		haveSeg = false
	}

	for _, rawLine := range rawLines {
		lineStart := pos
		pos += int64(len(rawLine)) + 1
		line := strings.TrimSpace(rawLine)
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
		// A record with no timestamp of its own inherits the previous
		// one, so meta records cannot look like a jump to the zero time.
		at, ok := rec.at()
		if !ok {
			at = prevAt
		}

		if haveSeg {
			gapCut := gap > 0 && !prevAt.IsZero() && !at.IsZero() && at.Sub(prevAt) > gap
			sizeCut := maxBytes > 0 && sb.Len() > maxBytes
			if gapCut || sizeCut {
				flush(lineStart)
			}
		}
		if !haveSeg {
			segAt = at
			haveSeg = true
		}
		sb.WriteString(role)
		sb.WriteString(": ")
		sb.WriteString(text)
		sb.WriteString("\n")
		prevAt = at
	}
	flush(pos)

	// A short tail is not its own idea. Merge it backwards rather than
	// spending a gate evaluation to discover that. The merged segment
	// keeps the tail's EndOffset: it now carries the tail's bytes too, and
	// the predecessor's old EndOffset would leave them looking unconsumed.
	if len(segs) > 1 && len(segs[len(segs)-1].Content) < minSegmentBytes {
		last := segs[len(segs)-1]
		segs = segs[:len(segs)-1]
		segs[len(segs)-1].Content += "\n" + last.Content
		segs[len(segs)-1].EndOffset = last.EndOffset
	}
	return segs, offset + int64(consumed), nil
}

// transcriptRecord matches the loose schema Claude Code writes to the
// transcript JSONL. Each line is either a wrapped message block (role plus
// content) or a top-level type indicator.
type transcriptRecord struct {
	Type      string `json:"type,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Message   *struct {
		Role    string          `json:"role,omitempty"`
		Content json.RawMessage `json:"content,omitempty"`
	} `json:"message,omitempty"`
}

// at parses the record's timestamp. Summaries and meta records carry none.
func (r transcriptRecord) at() (time.Time, bool) {
	if r.Timestamp == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, r.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
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
	// Content is either a string or an array of typed blocks. Keep text.
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

// ingestPayload mirrors httpapi.IngestRequest.
type ingestPayload struct {
	Kind         string         `json:"kind"`
	Title        string         `json:"title,omitempty"`
	ExternalRef  string         `json:"external_ref,omitempty"`
	Content      string         `json:"content"`
	Participants []string       `json:"participants,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	User         string         `json:"user,omitempty"`
	Project      string         `json:"project,omitempty"`
	OccurredAt   *time.Time     `json:"occurred_at,omitempty"`
}

// ─── transcript offsets ──────────────────────────────────────────────

type offsetRecord struct {
	Path    string    `json:"path"`
	Offset  int64     `json:"offset"`
	Updated time.Time `json:"updated"`
}

// offsetFile is the per-session state path. Session ids come from Claude
// Code and are used in a filename, so they are sanitised.
func offsetFile(sessionID string) (string, error) {
	dir, err := offsetsDir()
	if err != nil {
		return "", err
	}
	name := sanitizeSlug(sessionID)
	if name == "" {
		name = "unknown-session"
	}
	return filepath.Join(dir, name+".json"), nil
}

func readOffset(sessionID, transcriptPath string) int64 {
	path, err := offsetFile(sessionID)
	if err != nil {
		return 0
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var rec offsetRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return 0
	}
	// A different transcript under the same session id means the offset
	// does not apply.
	if rec.Path != transcriptPath {
		return 0
	}
	return rec.Offset
}

func writeOffset(sessionID, transcriptPath string, offset int64) error {
	path, err := offsetFile(sessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(offsetRecord{Path: transcriptPath, Offset: offset, Updated: time.Now().UTC()})
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	pruneOffsets()
	return nil
}

// pruneOffsets deletes offset files for sessions that ended long ago, so
// the directory does not grow without bound.
func pruneOffsets() {
	dir, err := offsetsDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// ─── hook log ────────────────────────────────────────────────────────

// hookLogEntry is one line of ~/.anamnesia/hooks.log.
type hookLogEntry struct {
	At    time.Time `json:"at"`
	Verb  string    `json:"verb"`
	OK    bool      `json:"ok"`
	Ms    int64     `json:"ms"`
	Note  string    `json:"note,omitempty"`
	Error string    `json:"error,omitempty"`
}

// hookLogMaxBytes caps the log; past it, the oldest half is dropped.
const hookLogMaxBytes = 512 << 10

// logHook records the outcome of a hook run. Best-effort by design: a hook
// must not fail because its own logging failed.
func logHook(verb string, started time.Time, runErr error, note string) {
	path, err := hookLogPath()
	if err != nil {
		return
	}
	if _, err := ensureHome(); err != nil {
		return
	}
	entry := hookLogEntry{
		At:   time.Now().UTC(),
		Verb: verb,
		OK:   runErr == nil,
		Ms:   time.Since(started).Milliseconds(),
		Note: note,
	}
	if runErr != nil {
		entry.Error = runErr.Error()
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	rotateHookLog(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

func rotateHookLog(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < hookLogMaxBytes {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	half := raw[len(raw)/2:]
	if i := bytes.IndexByte(half, '\n'); i >= 0 {
		half = half[i+1:]
	}
	_ = os.WriteFile(path, half, 0o600)
}

// readHookLogTail returns the last n entries, newest last.
func readHookLogTail(n int) ([]hookLogEntry, error) {
	path, err := hookLogPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var out []hookLogEntry
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var e hookLogEntry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// trimLine takes the first line of s, truncated to at most n runes.
func trimLine(s string, n int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n]) + "…"
	}
	return s
}
