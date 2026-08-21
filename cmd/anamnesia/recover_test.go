package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedStranded writes a transcript of `size` bytes last modified `age`
// ago, plus an offset file claiming `read` bytes of it.
func seedStranded(t *testing.T, session string, size, read int64, age time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	tp := filepath.Join(dir, session+".jsonl")
	if err := os.WriteFile(tp, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(tp, when, when); err != nil {
		t.Fatal(err)
	}
	od, err := offsetsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(od, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(offsetRecord{Path: tp, Offset: read, Updated: when.UTC()})
	if err := os.WriteFile(filepath.Join(od, session+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return tp
}

func strandedPaths(t *testing.T, idle time.Duration) map[string]bool {
	t.Helper()
	got, err := strandedSessions(idle)
	if err != nil {
		t.Fatalf("strandedSessions: %v", err)
	}
	out := map[string]bool{}
	for _, s := range got {
		out[s.Path] = true
	}
	return out
}

// TestAnIdleTranscriptWithUnreadBytesIsStranded is the case the command
// exists for: a session that died without a SessionEnd. The transcript
// survives on disk with the offset recording exactly how far anamnesia
// got, so nothing is lost — but until now nothing ever went back for it.
func TestAnIdleTranscriptWithUnreadBytesIsStranded(t *testing.T) {
	isolatedHome(t)
	tp := seedStranded(t, "dead-session", 5000, 1000, time.Hour)

	if !strandedPaths(t, 15*time.Minute)[tp] {
		t.Error("a transcript idle for an hour with 4000 unread bytes was not offered for recovery")
	}
}

// TestALiveTranscriptIsNotStranded. The idle window is the whole safety
// story: a session being written to right now is not abandoned, and
// ingesting its tail would both race the live checkpoint and pay for
// extraction over content that is about to be sent again.
func TestALiveTranscriptIsNotStranded(t *testing.T) {
	isolatedHome(t)
	tp := seedStranded(t, "live-session", 5000, 1000, 10*time.Second)

	if strandedPaths(t, 15*time.Minute)[tp] {
		t.Error("a transcript written 10 seconds ago was treated as abandoned")
	}
}

// TestAFullyReadTranscriptIsNotStranded: the ordinary case, and the one
// that must stay cheap. Most sessions end cleanly.
func TestAFullyReadTranscriptIsNotStranded(t *testing.T) {
	isolatedHome(t)
	tp := seedStranded(t, "clean-session", 5000, 5000, time.Hour)

	if strandedPaths(t, 15*time.Minute)[tp] {
		t.Error("a transcript read to the end was offered for recovery")
	}
}

// TestATranscriptThatNoLongerExistsIsNotStranded: offset files outlive
// the transcripts they point at.
func TestATranscriptThatNoLongerExistsIsNotStranded(t *testing.T) {
	isolatedHome(t)
	tp := seedStranded(t, "gone-session", 5000, 1000, time.Hour)
	if err := os.Remove(tp); err != nil {
		t.Fatal(err)
	}

	if strandedPaths(t, 15*time.Minute)[tp] {
		t.Error("an offset pointing at a deleted transcript was offered for recovery")
	}
}

// TestATruncatedTranscriptIsNotStranded: a transcript shorter than the
// offset is not a tail to read, it is a file that was replaced or
// truncated. Reading "from 1000 to 500" is not a recovery.
func TestATruncatedTranscriptIsNotStranded(t *testing.T) {
	isolatedHome(t)
	tp := seedStranded(t, "short-session", 500, 1000, time.Hour)

	if strandedPaths(t, 15*time.Minute)[tp] {
		t.Error("a transcript shorter than its recorded offset was offered for recovery")
	}
}

// TestTheCWDComesFromTheTranscript. A recovered tail has to be filed
// under the project it came from, not under wherever `recover` happens to
// run. The offset file records only the transcript path, and the
// directory name in that path is the working directory with every
// separator flattened to a dash, which cannot be reversed: hub-2.0/hub-api
// becomes hub-2-0-hub-api. The transcript itself records the real cwd.
func TestTheCWDComesFromTheTranscript(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "t.jsonl")
	lines := `{"type":"last-prompt","sessionId":"x"}
{"type":"user","cwd":"/Users/floh/Work/smoxy/hub-2.0/hub-api","message":{"role":"user"}}
`
	if err := os.WriteFile(tp, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := transcriptCWD(tp); got != "/Users/floh/Work/smoxy/hub-2.0/hub-api" {
		t.Errorf("transcriptCWD = %q, want the recorded working directory", got)
	}
}

// TestCWDIsEmptyWhenTheTranscriptDoesNotSayOne, so the caller can skip
// rather than guess a project.
func TestCWDIsEmptyWhenTheTranscriptDoesNotSayOne(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(tp, []byte("{\"type\":\"last-prompt\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := transcriptCWD(tp); got != "" {
		t.Errorf("transcriptCWD = %q, want empty", got)
	}
}

// TestProjectForDirPrefersTheProjectFile mirrors what a hook running in
// that directory would resolve.
func TestProjectForDirPrefersTheProjectFile(t *testing.T) {
	dir := t.TempDir()
	body := "[identity]\nproject = \"smoxy\"\n"
	if err := os.WriteFile(filepath.Join(dir, projectConfigName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := projectForDir(dir); got != "smoxy" {
		t.Errorf("projectForDir = %q, want smoxy", got)
	}
}

// TestProjectForDirFallsBackToTheDirectoryName, which is what
// identity.project defaults to.
func TestProjectForDirFallsBackToTheDirectoryName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hub-api")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := projectForDir(dir); got != "hub-api" {
		t.Errorf("projectForDir = %q, want hub-api", got)
	}
}
