package main

import (
	"encoding/json"
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

	text, offset, err := readTranscriptFrom(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "first question") {
		t.Fatalf("first read missed content: %q", text)
	}
	if offset == 0 {
		t.Fatal("offset did not advance")
	}

	// Nothing new: a second checkpoint must send nothing at all.
	text, sameOffset, err := readTranscriptFrom(path, offset)
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		t.Errorf("re-read returned content already sent: %q", text)
	}
	if sameOffset != offset {
		t.Errorf("offset moved without new content: %d → %d", offset, sameOffset)
	}

	// Only the new turn should come back.
	appendTranscript(t, path, transcriptLine(t, "assistant", "second answer"))
	text, newOffset, err := readTranscriptFrom(path, offset)
	if err != nil {
		t.Fatal(err)
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

	text, _, err := readTranscriptFrom(path, 10_000)
	if err != nil {
		t.Fatal(err)
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

	text, offset, err := readTranscriptFrom(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "complete turn") {
		t.Errorf("complete line missing: %q", text)
	}
	if offset != int64(len(complete)) {
		t.Errorf("offset = %d, want %d (only whole lines may be consumed)", offset, len(complete))
	}

	// Finishing the line makes it available on the next read.
	appendTranscript(t, path, `","content":[{"type":"text","text":"finished"}]}}`+"\n")
	text, _, err = readTranscriptFrom(path, offset)
	if err != nil {
		t.Fatal(err)
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

	text, _, err := readTranscriptFrom(path, 0)
	if err != nil {
		t.Fatal(err)
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
