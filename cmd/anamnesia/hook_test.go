package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// appendSegLines appends more turns to an existing transcript, the way a
// session that keeps going after a checkpoint would.
func appendSegLines(t *testing.T, path string, turns ...[3]string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
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
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
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

// TestMergedTrailingSegmentKeepsTheTailsEndOffset: the merge folds the
// tail's content into its predecessor, so its EndOffset must move too - if
// it kept the predecessor's old EndOffset, the tail's bytes would look
// unconsumed and be re-sent on the next checkpoint.
func TestMergedTrailingSegmentKeepsTheTailsEndOffset(t *testing.T) {
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", strings.Repeat("a substantial first exchange. ", 20)},
		[3]string{"2026-03-02T10:00:00Z", "user", "ok"},
	)
	segs, next, err := readTranscriptFrom(path, 0, 20*time.Minute, 32768)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Fatalf("segments = %d, want 1", len(segs))
	}
	if segs[0].EndOffset != next {
		t.Errorf("merged segment EndOffset = %d, want it to reach the tail's end %d", segs[0].EndOffset, next)
	}
}

// TestSegmentEndOffsetsAscendToNext: EndOffset is computed by walking the
// same lines readTranscriptFrom already walks to produce next. If the two
// ever disagree, one of the two accounting paths has a bug.
func TestSegmentEndOffsetsAscendToNext(t *testing.T) {
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "why are the stock counts off"},
		[3]string{"2026-03-02T09:01:00Z", "assistant", "the Rotterdam site writes local time"},
		[3]string{"2026-03-02T09:41:00Z", "user", "unrelated: the invoice PDF job runs out of memory"},
		[3]string{"2026-03-02T09:42:00Z", "assistant", "stream it page by page"},
	)
	segs, next, err := readTranscriptFrom(path, 0, 20*time.Minute, 32768)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Fatalf("segments = %d, want 2", len(segs))
	}
	if segs[0].EndOffset <= 0 || segs[0].EndOffset >= segs[1].EndOffset {
		t.Errorf("EndOffsets do not ascend: %d, %d", segs[0].EndOffset, segs[1].EndOffset)
	}
	if segs[len(segs)-1].EndOffset != next {
		t.Errorf("last segment EndOffset = %d, want it to equal next = %d", segs[len(segs)-1].EndOffset, next)
	}
}

// captureIngestSourceID returns the source_id captureIngests's test server
// hands back for its nth request (1-indexed), so a test can predict what
// id a given post received without re-deriving the format itself.
func captureIngestSourceID(n int) string {
	return fmt.Sprintf("6f1c1f7a-0a1a-4a7a-9a7a-%012d", n)
}

// captureIngests stands up a server that records every ingest body, and
// points a host config at it. Each response carries a distinct source_id
// (see captureIngestSourceID) — a fixed one would let every segment of a
// checkpoint look like the same source, which is exactly the bug the
// graph bridge needs a test to catch.
func captureIngests(t *testing.T) (*hostConfig, *[]map[string]any) {
	t.Helper()
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"source_id":%q,"queued":true}`, captureIngestSourceID(len(got)))
	}))
	t.Cleanup(srv.Close)
	hc := testHostConfig(t)
	hc.values["server.url"] = srv.URL
	return hc, &got
}

func TestCheckpointPostsOneSourcePerSegment(t *testing.T) {
	hc, got := captureIngests(t)
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "the first subject, discussed at some length this morning"},
		[3]string{"2026-03-02T09:41:00Z", "user", "an entirely separate subject, also discussed at length"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "s1"}, "claude-session", checkpointScope{}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if len(*got) != 2 {
		t.Fatalf("posted %d sources, want one per segment", len(*got))
	}
	if (*got)[0]["occurred_at"] == nil {
		t.Error("segment posted without an occurred_at; decay reads it")
	}
	first, _ := (*got)[0]["external_ref"].(string)
	second, _ := (*got)[1]["external_ref"].(string)
	if first == second || !strings.HasPrefix(first, "s1") {
		t.Errorf("external_refs = %q, %q; want the session id with distinct suffixes", first, second)
	}
}

// TestCheckpointAdvancesTheOffsetOnlyWhenEverySegmentLands: a checkpoint
// that runs out of time must be able to catch up. If the offset stayed put
// on any failure, the next checkpoint would re-read the same range plus
// whatever the session added since, making the range - and the time it
// takes to fail again - grow without bound. So the offset advances as far
// as it safely can: past every segment that landed, short of the one that
// didn't. Losing a segment is worse than sending one twice: the gate
// deduplicates a repeat, and nothing recovers a segment that was skipped.
func TestCheckpointAdvancesTheOffsetOnlyWhenEverySegmentLands(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if posts == 2 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"source_id":"6f1c1f7a-0a1a-4a7a-9a7a-1a7a1a7a1a7a","queued":true}`))
	}))
	t.Cleanup(srv.Close)
	hc := testHostConfig(t)
	hc.values["server.url"] = srv.URL

	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "the first subject, discussed at some length this morning"},
		[3]string{"2026-03-02T09:41:00Z", "user", "a second subject, also discussed at length"},
		[3]string{"2026-03-02T10:22:00Z", "user", "a third subject, discussed at length as well"},
	)
	wantSegs, next, err := readTranscriptFrom(path, 0, hc.Dur("ingest.segment_gap"), hc.Int("ingest.segment_max_bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if len(wantSegs) != 3 {
		t.Fatalf("test setup: got %d segments, want 3", len(wantSegs))
	}

	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "s2"}, "claude-session", checkpointScope{}); err == nil {
		t.Fatal("checkpoint reported success despite a failed segment")
	}
	off := readOffset("s2", path)
	if off != wantSegs[0].EndOffset {
		t.Errorf("offset = %d, want the first segment's EndOffset %d", off, wantSegs[0].EndOffset)
	}
	if off == 0 {
		t.Error("offset stayed at 0 despite the first segment landing")
	}
	if off == next {
		t.Error("offset advanced as far as full success, but the third segment was never sent")
	}
}

// TestCheckpointOffsetStaysPutWhenTheFirstSegmentFails: with nothing
// posted yet, there is no "last good" position to advance to, so the
// offset must be left exactly where it started.
func TestCheckpointOffsetStaysPutWhenTheFirstSegmentFails(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if posts == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"source_id":"6f1c1f7a-0a1a-4a7a-9a7a-1a7a1a7a1a7a","queued":true}`))
	}))
	t.Cleanup(srv.Close)
	hc := testHostConfig(t)
	hc.values["server.url"] = srv.URL

	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "the first subject, discussed at some length this morning"},
		[3]string{"2026-03-02T09:41:00Z", "user", "an entirely separate subject, also discussed at length"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "s5"}, "claude-session", checkpointScope{}); err == nil {
		t.Fatal("checkpoint reported success despite a failed segment")
	}
	if off := readOffset("s5", path); off != 0 {
		t.Errorf("offset = %d after the first segment failed, want 0", off)
	}
}

// TestSegmentWithNoTimestampOmitsOccurredAt: a segment whose first record
// carries no parsable timestamp has a zero time.Time as its At. Posting
// that as occurred_at would record the memory as having happened in year
// 1, and decay reads occurred_at, so it would be treated as infinitely
// stale. It must be left out of the payload entirely rather than sent as
// a zero time.
func TestSegmentWithNoTimestampOmitsOccurredAt(t *testing.T) {
	hc, got := captureIngests(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	body := `{"type":"user","message":{"role":"user","content":"a question with no timestamp on its record at all"}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "s3"}, "claude-session", checkpointScope{}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("posted %d sources, want 1", len(*got))
	}
	if _, present := (*got)[0]["occurred_at"]; present {
		t.Errorf("occurred_at present with no source timestamp: %v", (*got)[0]["occurred_at"])
	}
}

// TestCheckpointByteRangesAreContiguousPerSegment: byte_range is the field
// that maps a stored memory back to a place in the transcript. Every
// segment in a checkpoint must carry its own range, not the whole batch's,
// or all but the last segment lie about where they came from.
func TestCheckpointByteRangesAreContiguousPerSegment(t *testing.T) {
	hc, got := captureIngests(t)
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "the first subject, discussed at some length this morning"},
		[3]string{"2026-03-02T09:41:00Z", "user", "an entirely separate subject, also discussed at length"},
	)
	wantSegs, next, err := readTranscriptFrom(path, 0, hc.Dur("ingest.segment_gap"), hc.Int("ingest.segment_max_bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if len(wantSegs) != 2 {
		t.Fatalf("test setup: got %d segments, want 2", len(wantSegs))
	}

	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "s6"}, "claude-session", checkpointScope{}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if len(*got) != 2 {
		t.Fatalf("posted %d sources, want 2", len(*got))
	}

	firstRange, _ := (*got)[0]["metadata"].(map[string]any)["byte_range"].(string)
	secondRange, _ := (*got)[1]["metadata"].(map[string]any)["byte_range"].(string)
	wantFirst := fmt.Sprintf("0-%d", wantSegs[0].EndOffset)
	wantSecond := fmt.Sprintf("%d-%d", wantSegs[0].EndOffset, wantSegs[1].EndOffset)
	if firstRange != wantFirst {
		t.Errorf("first byte_range = %q, want %q", firstRange, wantFirst)
	}
	if secondRange != wantSecond {
		t.Errorf("second byte_range = %q, want %q", secondRange, wantSecond)
	}
	if wantSegs[1].EndOffset != next {
		t.Fatalf("test setup: last segment EndOffset %d != next %d", wantSegs[1].EndOffset, next)
	}
}

// TestCheckpointExternalRefsDoNotCollideAcrossCheckpoints is the point of
// the fix: external_ref used to be "<session>#<index within this
// checkpoint>", so a second checkpoint of the same session reused the
// first checkpoint's refs for entirely different content. Keying on the
// segment's starting byte offset instead makes every ref unique across the
// whole session, because offsets never repeat.
func TestCheckpointExternalRefsDoNotCollideAcrossCheckpoints(t *testing.T) {
	hc, got := captureIngests(t)
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "the first subject, discussed at some length this morning"},
		[3]string{"2026-03-02T09:41:00Z", "user", "an entirely separate subject, also discussed at length"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "s7"}, "claude-session", checkpointScope{}); err != nil {
		t.Fatalf("first checkpoint: %v", err)
	}
	if len(*got) != 2 {
		t.Fatalf("posted %d sources after the first checkpoint, want 2", len(*got))
	}

	appendSegLines(t, path,
		[3]string{"2026-03-02T10:30:00Z", "user", "a third subject, raised well after the first checkpoint"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "s7"}, "claude-session", checkpointScope{}); err != nil {
		t.Fatalf("second checkpoint: %v", err)
	}
	if len(*got) != 3 {
		t.Fatalf("posted %d sources total, want 3", len(*got))
	}

	seen := map[string]bool{}
	for _, body := range *got {
		ref, _ := body["external_ref"].(string)
		if ref == "" {
			t.Fatal("external_ref missing")
		}
		if seen[ref] {
			t.Errorf("external_ref %q reused across checkpoints", ref)
		}
		seen[ref] = true
	}
}

// TestCheckpointByteRangeStartsAtResumeOffsetNotZero: the first segment of
// a checkpoint that resumes mid-transcript must report its range starting
// where the transcript was last left off, not from byte 0.
func TestCheckpointByteRangeStartsAtResumeOffsetNotZero(t *testing.T) {
	hc, got := captureIngests(t)
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "the first subject, discussed at some length this morning"},
		[3]string{"2026-03-02T09:41:00Z", "user", "an entirely separate subject, also discussed at length"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "s8"}, "claude-session", checkpointScope{}); err != nil {
		t.Fatalf("first checkpoint: %v", err)
	}
	resumeOffset := readOffset("s8", path)
	if resumeOffset == 0 {
		t.Fatal("test setup: offset did not advance after the first checkpoint")
	}
	*got = nil // isolate the second checkpoint's payloads

	appendSegLines(t, path,
		[3]string{"2026-03-02T10:30:00Z", "user", "a third subject, raised well after the first checkpoint"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "s8"}, "claude-session", checkpointScope{}); err != nil {
		t.Fatalf("second checkpoint: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("posted %d sources on the second checkpoint, want 1", len(*got))
	}
	wantPrefix := fmt.Sprintf("%d-", resumeOffset)
	gotRange, _ := (*got)[0]["metadata"].(map[string]any)["byte_range"].(string)
	if !strings.HasPrefix(gotRange, wantPrefix) {
		t.Errorf("byte_range = %q, want it to start at the resume offset %d", gotRange, resumeOffset)
	}
}

func TestGraphSourceIsPostedAfterTheSegments(t *testing.T) {
	hc, got := captureIngests(t)
	hc.values["graph.extract"] = "true"
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "the first subject, discussed at some length this morning"},
		[3]string{"2026-03-02T09:41:00Z", "user", "an entirely separate subject, also discussed at length"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "g1"}, "claude-session", checkpointScope{}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if len(*got) != 3 {
		t.Fatalf("posted %d sources, want 2 segments plus 1 graph source", len(*got))
	}
	last := (*got)[2]
	if last["kind"] != "claude-session-graph" {
		t.Errorf("last source kind = %v, want the graph kind", last["kind"])
	}
	content, _ := last["content"].(string)
	if !strings.Contains(content, "first subject") || !strings.Contains(content, "separate subject") {
		t.Errorf("graph source does not carry the whole checkpoint:\n%s", content)
	}
}

// TestGraphSourceMetadataCarriesSegmentSourceIDs is the point of the fix:
// runGraph records mentions against whatever ids the graph source's
// metadata names, and this is the hook's half of that bridge — without
// it, the graph source carries no way back to the sources a search hit
// actually returns. See
// docs/superpowers/specs/2026-08-19-the-graph-bridge-is-broken.md.
func TestGraphSourceMetadataCarriesSegmentSourceIDs(t *testing.T) {
	hc, got := captureIngests(t)
	hc.values["graph.extract"] = "true"
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "the first subject, discussed at some length this morning"},
		[3]string{"2026-03-02T09:41:00Z", "user", "an entirely separate subject, also discussed at length"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "g4"}, "claude-session", checkpointScope{}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if len(*got) != 3 {
		t.Fatalf("posted %d sources, want 2 segments plus 1 graph source", len(*got))
	}
	meta, _ := (*got)[2]["metadata"].(map[string]any)
	gotIDs, _ := meta["segment_source_ids"].([]any)
	wantIDs := []any{captureIngestSourceID(1), captureIngestSourceID(2)}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("segment_source_ids = %v, want %v", gotIDs, wantIDs)
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Errorf("segment_source_ids[%d] = %v, want %v", i, gotIDs[i], wantIDs[i])
		}
	}
}

func TestNoGraphSourceWhenTheFlagIsOff(t *testing.T) {
	hc, got := captureIngests(t)
	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "the first subject, discussed at some length this morning"},
		[3]string{"2026-03-02T09:41:00Z", "user", "an entirely separate subject, also discussed at length"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "g2"}, "claude-session", checkpointScope{}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if len(*got) != 2 {
		t.Errorf("posted %d sources with graph.extract off, want just the 2 segments", len(*got))
	}
}

func TestAFailedGraphSourceDoesNotFailTheCheckpoint(t *testing.T) {
	// The segments are the memory. The graph source is an extra, and losing
	// it must not hold back the offset and re-send everything next time.
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if posts == 3 { // the graph source, after two segments
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"source_id":"6f1c1f7a-0a1a-4a7a-9a7a-1a7a1a7a1a7a","queued":true}`))
	}))
	t.Cleanup(srv.Close)
	hc := testHostConfig(t)
	hc.values["server.url"] = srv.URL
	hc.values["graph.extract"] = "true"

	path := segLines(t,
		[3]string{"2026-03-02T09:00:00Z", "user", "the first subject, discussed at some length this morning"},
		[3]string{"2026-03-02T09:41:00Z", "user", "an entirely separate subject, also discussed at length"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "g3"}, "claude-session", checkpointScope{}); err != nil {
		t.Errorf("a failed graph source failed the whole checkpoint: %v", err)
	}
	if off := readOffset("g3", path); off == 0 {
		t.Error("the offset was held back by a failed graph source; the segments all landed")
	}
}
