package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// transcriptLine renders one JSONL record the way Claude Code writes them.
func transcriptLine(t *testing.T, role, text string) string {
	t.Helper()
	rec := map[string]any{
		"type": role,
		"message": map[string]any{
			"role":    role,
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

func writeTranscript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendTranscript(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
}

// TestReadTranscriptFromIsIncremental is the regression test for the
// quadratic ingest: checkpointing used to re-read the whole transcript, so a
// long session was re-sent and re-extracted from the beginning every time.
func TestReadTranscriptFromIsIncremental(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	writeTranscript(t, path, transcriptLine(t, "user", "first question"))

	segs, offset, err := readTranscriptFrom(path, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	if len(segs) > 0 {
		text = segs[0].Content
	}
	if !strings.Contains(text, "first question") {
		t.Fatalf("first read missed content: %q", text)
	}
	if offset == 0 {
		t.Fatal("offset did not advance")
	}

	// Nothing new: a second checkpoint must send nothing at all.
	segs, sameOffset, err := readTranscriptFrom(path, offset, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	text = ""
	if len(segs) > 0 {
		text = segs[0].Content
	}
	if text != "" {
		t.Errorf("re-read returned content already sent: %q", text)
	}
	if sameOffset != offset {
		t.Errorf("offset moved without new content: %d → %d", offset, sameOffset)
	}

	// Only the new turn should come back.
	appendTranscript(t, path, transcriptLine(t, "assistant", "second answer"))
	segs, newOffset, err := readTranscriptFrom(path, offset, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	text = ""
	if len(segs) > 0 {
		text = segs[0].Content
	}
	if strings.Contains(text, "first question") {
		t.Errorf("already-sent content repeated: %q", text)
	}
	if !strings.Contains(text, "second answer") {
		t.Errorf("new content missing: %q", text)
	}
	if newOffset <= offset {
		t.Errorf("offset did not advance: %d → %d", offset, newOffset)
	}
}

// TestReadTranscriptFromHandlesReplacedFile: a shorter file means the
// transcript was replaced, so the stored offset is meaningless.
func TestReadTranscriptFromHandlesReplacedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	writeTranscript(t, path, transcriptLine(t, "user", "brand new session"))

	segs, _, err := readTranscriptFrom(path, 10_000, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	if len(segs) > 0 {
		text = segs[0].Content
	}
	if !strings.Contains(text, "brand new session") {
		t.Errorf("content lost after the file shrank: %q", text)
	}
}

// TestReadTranscriptFromIgnoresPartialLines: a checkpoint can land while
// Claude Code is mid-write, and consuming half a line would corrupt the
// next read.
func TestReadTranscriptFromIgnoresPartialLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	complete := transcriptLine(t, "user", "complete turn")
	writeTranscript(t, path, complete+`{"type":"assistant","message":{"role":"assi`)

	segs, offset, err := readTranscriptFrom(path, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	if len(segs) > 0 {
		text = segs[0].Content
	}
	if !strings.Contains(text, "complete turn") {
		t.Errorf("complete line missing: %q", text)
	}
	if offset != int64(len(complete)) {
		t.Errorf("offset = %d, want %d (only whole lines may be consumed)", offset, len(complete))
	}

	// Finishing the line makes it available on the next read.
	appendTranscript(t, path, `","content":[{"type":"text","text":"finished"}]}}`+"\n")
	segs, _, err = readTranscriptFrom(path, offset, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	text = ""
	if len(segs) > 0 {
		text = segs[0].Content
	}
	if !strings.Contains(text, "finished") {
		t.Errorf("completed line not picked up: %q", text)
	}
}

// TestReadTranscriptFromSkipsToolNoise keeps the ingest to prose: the
// assistant's own text already describes what the tools did.
func TestReadTranscriptFromSkipsToolNoise(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	toolUse, err := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "ls"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, path,
		transcriptLine(t, "user", "run it")+
			string(toolUse)+"\n"+
			`{"type":"system","subtype":"hook"}`+"\n"+
			transcriptLine(t, "assistant", "done, it worked"))

	segs, _, err := readTranscriptFrom(path, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	if len(segs) > 0 {
		text = segs[0].Content
	}
	if strings.Contains(text, "tool_use") || strings.Contains(text, "ls") {
		t.Errorf("tool traffic leaked into the ingest: %q", text)
	}
	for _, want := range []string{"run it", "done, it worked"} {
		if !strings.Contains(text, want) {
			t.Errorf("prose missing (%q): %q", want, text)
		}
	}
}

func TestOffsetPersistence(t *testing.T) {
	t.Setenv(homeEnv, t.TempDir())

	if got := readOffset("session-1", "/tmp/a.jsonl"); got != 0 {
		t.Errorf("fresh offset = %d, want 0", got)
	}
	if err := writeOffset("session-1", "/tmp/a.jsonl", 512); err != nil {
		t.Fatal(err)
	}
	if got := readOffset("session-1", "/tmp/a.jsonl"); got != 512 {
		t.Errorf("offset = %d, want 512", got)
	}
	// A different transcript under the same id must not inherit the offset.
	if got := readOffset("session-1", "/tmp/other.jsonl"); got != 0 {
		t.Errorf("offset leaked across transcripts: %d", got)
	}
	// A different session likewise starts fresh.
	if got := readOffset("session-2", "/tmp/a.jsonl"); got != 0 {
		t.Errorf("offset leaked across sessions: %d", got)
	}
}

// TestOffsetFileNameIsSafe: session ids arrive from outside and are used in
// a path.
func TestOffsetFileNameIsSafe(t *testing.T) {
	home := t.TempDir()
	t.Setenv(homeEnv, home)

	for _, id := range []string{"../../escape", "a/b/c", "", "..", "with space"} {
		path, err := offsetFile(id)
		if err != nil {
			t.Fatal(err)
		}
		dir, err := offsetsDir()
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(path) != dir {
			t.Errorf("session id %q escaped the offsets directory: %s", id, path)
		}
	}
}

// TestHookLogRoundTrip: the log is what lets doctor tell a working install
// from one whose hooks fail on every turn.
func TestHookLogRoundTrip(t *testing.T) {
	t.Setenv(homeEnv, t.TempDir())

	entries, err := readHookLogTail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected an empty log, got %d entries", len(entries))
	}

	logHook("retrieve", time.Now(), nil, "3 hits")
	logHook("session-end", time.Now(), os.ErrPermission, "")

	entries, err = readHookLogTail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if !entries[0].OK || entries[0].Verb != "retrieve" || entries[0].Note != "3 hits" {
		t.Errorf("first entry wrong: %+v", entries[0])
	}
	if entries[1].OK || entries[1].Error == "" {
		t.Errorf("failure not recorded: %+v", entries[1])
	}
}

func TestTrimLineHandlesMultibyte(t *testing.T) {
	// Slicing bytes would split a rune and emit invalid UTF-8.
	long := strings.Repeat("é", 100)
	got := trimLine(long, 10)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected an ellipsis: %q", got)
	}
	if runes := []rune(strings.TrimSuffix(got, "…")); len(runes) != 10 {
		t.Errorf("kept %d runes, want 10", len(runes))
	}
	if !utf8.ValidString(got) {
		t.Errorf("produced invalid UTF-8: %q", got)
	}
	if got := trimLine("first\nsecond", 100); got != "first" {
		t.Errorf("trimLine kept more than the first line: %q", got)
	}
}

func TestEveryHookVerbIsRoutable(t *testing.T) {
	for _, h := range anamnesiaHooks {
		if _, ok := hookTimeouts[h.verb]; !ok {
			t.Errorf("hook verb %q has no timeout configured", h.verb)
		}
		if h.timeout <= 0 {
			t.Errorf("hook %s has no Claude Code timeout set", h.event)
		}
	}
}

// writePIDForTest records this test process as the running server, with a
// chosen start time.
func writePIDForTest(t *testing.T, startedAt time.Time) {
	t.Helper()
	if err := writePID(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	path, err := serverPIDPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, startedAt, startedAt); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorIgnoresHookFailuresFromBeforeTheServerStarted covers a false alarm
// every first install would otherwise produce: hooks fire before the server
// exists, so the newest recorded run of a verb is a failure that starting the
// server has already resolved.
func TestDoctorIgnoresHookFailuresFromBeforeTheServerStarted(t *testing.T) {
	t.Setenv(homeEnv, t.TempDir())

	failedAt := time.Now().Add(-10 * time.Minute)
	logHook("retrieve", failedAt, errServerUnreachable, "")
	// Rewrite the entry's timestamp: logHook stamps "now".
	path, err := hookLogPath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var e hookLogEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &e); err != nil {
		t.Fatal(err)
	}
	e.At = failedAt
	fixed, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(fixed, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// Server started after that failure: the fault is history.
	writePIDForTest(t, failedAt.Add(time.Minute))
	if got := checkHookActivity(); got.Status == statusFail {
		t.Errorf("a failure predating the server start was reported as a fault: %+v", got)
	}

	// Server started before it: still outstanding, so still a failure.
	writePIDForTest(t, failedAt.Add(-time.Minute))
	if got := checkHookActivity(); got.Status != statusFail {
		t.Errorf("an unresolved hook failure was not reported: %+v", got)
	}
}

// segLines builds a transcript JSONL from (minutesFromStart, role, text).
func segLines(t *testing.T, turns ...[3]string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	var sb strings.Builder
	for _, tr := range turns {
		rec := map[string]any{
			"type":      tr[1],
			"timestamp": tr[0],
			"message":   map[string]any{"role": tr[1], "content": tr[2]},
		}
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
		sb.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSegmentsCutOnALongGap(t *testing.T) {
	// Two exchanges 40 minutes apart. A pause is the cheapest signal we have
	// that the subject changed.
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "why are the stock counts off"},
		[3]string{"2026-03-02T09:01:00Z", "assistant", "the Rotterdam site writes local time"},
		[3]string{"2026-03-02T09:41:00Z", "user", "unrelated: the invoice PDF job runs out of memory"},
		[3]string{"2026-03-02T09:42:00Z", "assistant", "stream it page by page"},
	)
	segs, _, err := readTranscriptFrom(path, 0, 20*time.Minute, 32768)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Fatalf("segments = %d, want 2 across a 40 minute gap:\n%#v", len(segs), segs)
	}
	if !strings.Contains(segs[0].Content, "Rotterdam") || strings.Contains(segs[0].Content, "invoice") {
		t.Errorf("first segment has the wrong turns:\n%s", segs[0].Content)
	}
	if !strings.Contains(segs[1].Content, "invoice") {
		t.Errorf("second segment has the wrong turns:\n%s", segs[1].Content)
	}
}

func TestSegmentsDoNotCutOnAShortGap(t *testing.T) {
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "why are the stock counts off by a day"},
		[3]string{"2026-03-02T09:05:00Z", "assistant", "the Rotterdam site writes local time, everything else UTC"},
	)
	segs, _, err := readTranscriptFrom(path, 0, 20*time.Minute, 32768)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Errorf("segments = %d, want 1 for a five minute pause", len(segs))
	}
}

func TestSegmentsCutOnSize(t *testing.T) {
	// A four hour debugging session has no gaps and still is not one idea.
	long := strings.Repeat("we traced the discrepancy through the reconciliation job. ", 20)
	var turns [][3]string
	for i := 0; i < 8; i++ {
		turns = append(turns, [3]string{
			fmt.Sprintf("2026-03-02T09:%02d:00Z", i), "user", long,
		})
	}
	path := segLines(t, turns...)
	segs, _, err := readTranscriptFrom(path, 0, 20*time.Minute, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) < 2 {
		t.Fatalf("segments = %d, want several under a 2000 byte ceiling", len(segs))
	}
	for i, s := range segs {
		if strings.Count(s.Content, "user:") == 0 {
			t.Errorf("segment %d has no whole turn in it:\n%s", i, s.Content)
		}
	}
}

func TestSegmentCarriesItsFirstTurnsTime(t *testing.T) {
	// An experience about an afternoon's work did not happen at the moment
	// the session closed, and decay reads occurred_at.
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "first thing we looked at this morning, in some detail"},
		[3]string{"2026-03-02T09:02:00Z", "assistant", "and the answer we reached about it, also in detail"},
		[3]string{"2026-03-02T14:00:00Z", "user", "a completely separate thing in the afternoon, at length"},
	)
	segs, _, err := readTranscriptFrom(path, 0, 20*time.Minute, 32768)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Fatalf("segments = %d, want 2", len(segs))
	}
	if got, want := segs[0].At.UTC().Format(time.RFC3339), "2026-03-02T09:00:00Z"; got != want {
		t.Errorf("first segment At = %s, want its first turn %s", got, want)
	}
	if got, want := segs[1].At.UTC().Format(time.RFC3339), "2026-03-02T14:00:00Z"; got != want {
		t.Errorf("second segment At = %s, want its first turn %s", got, want)
	}
}

func TestRecordsWithoutATimestampInheritTheLastOne(t *testing.T) {
	// Summaries and meta records carry no timestamp. They must not look like
	// a jump back to the zero time and cut the transcript at every one.
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	body := `{"type":"user","timestamp":"2026-03-02T09:00:00Z","message":{"role":"user","content":"a question about the reconciliation job we ran"}}
{"type":"assistant","message":{"role":"assistant","content":"an answer with no timestamp on its record at all"}}
{"type":"user","timestamp":"2026-03-02T09:01:00Z","message":{"role":"user","content":"a follow up question one minute later"}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	segs, _, err := readTranscriptFrom(path, 0, 20*time.Minute, 32768)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Errorf("segments = %d, want 1: a missing timestamp must not force a cut", len(segs))
	}
}

func TestBothSettingsZeroRestoresASingleSegment(t *testing.T) {
	// The reversibility guarantee. An install that sets both to 0 gets exactly
	// today's behaviour, without a downgrade.
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "one thing we discussed at some length this morning"},
		[3]string{"2026-03-02T18:00:00Z", "user", "a completely different thing nine hours later, also at length"},
	)
	segs, _, err := readTranscriptFrom(path, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Errorf("segments = %d, want exactly 1 with both settings disabled", len(segs))
	}
}

func TestAShortTrailingSegmentMergesBackwards(t *testing.T) {
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", strings.Repeat("a substantial first exchange. ", 20)},
		[3]string{"2026-03-02T10:00:00Z", "user", "ok"},
	)
	segs, _, err := readTranscriptFrom(path, 0, 20*time.Minute, 32768)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Fatalf("segments = %d, want 1: a two word coda is not its own source", len(segs))
	}
	if !strings.Contains(segs[0].Content, "ok") {
		t.Errorf("the short tail was dropped instead of merged:\n%s", segs[0].Content)
	}
}
