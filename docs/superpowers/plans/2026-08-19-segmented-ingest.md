# Segmented Ingest Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cut a checkpoint into segments in the hook and post each as its own source, so the surprise gate judges ideas rather than whole days and the per-source operation cap applies per segment.

**Architecture:** All the work is in `cmd/anamnesia/hook.go`. `readTranscriptFrom` stops returning one string and returns a slice of segments, each carrying its own content and the timestamp of its first turn. `doCheckpoint` posts them in order and advances the byte offset only when every one is accepted. Nothing on the server changes; nothing about retrieval changes.

**Tech Stack:** Go 1.25+ (toolchain in the `golang:1.26` container — there is no Go on this host), `cobra`, standard library. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-18-segmented-ingest-design.md`

## Global Constraints

- **Run the toolchain in Docker.** No `go` or `gofmt` on this host:
  `docker run --rm -v "$PWD":/src:ro -v <scratch>/gocache:/gocache -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod -w /src golang:1.26 <cmd>`
  Drop `:ro` for `gofmt -w` or a build.
- `gofmt -s -l .` prints nothing; `go vet ./...`; `go test -race ./...` all pass.
- **Every setting is declared in `cmd/anamnesia/settings.go` and nowhere else.**
- **Never default silently.** A bad setting value is an error naming the setting.
- **Hooks never break a session.** They exit 0 whatever happens and record the outcome in `hooks.log`. Nothing in this plan may change that.
- **Both settings at `0` must restore today's behaviour exactly** — one segment for any input. This is the reversibility guarantee and it is a test, not a claim.
- No new third-party dependencies.
- Do not add "Co-Authored-By" or attribution lines to commits.

---

## File Structure

| File | Responsibility |
|---|---|
| `cmd/anamnesia/settings.go` | Declares `ingest.segment_gap` and `ingest.segment_max_bytes`. |
| `cmd/anamnesia/hook.go` | `segment` type, segmentation inside `readTranscriptFrom`, timestamp parsing on `transcriptRecord`, `OccurredAt` on `ingestPayload`, the posting loop in `doCheckpoint`. |
| `cmd/anamnesia/hook_test.go` | Segmentation rules and the posting loop. All no-database; the posting tests use `httptest`. |

---

### Task 1: Settings and segmentation

**Files:**
- Modify: `cmd/anamnesia/settings.go` (add two settings in a new `ingest` section, placed after the `worker` block)
- Modify: `cmd/anamnesia/hook.go`
- Test: `cmd/anamnesia/hook_test.go`

**Interfaces:**
- Consumes: `hostConfig.Dur(key)`, `hostConfig.Int(key)` (existing).
- Produces:
  - `type segment struct { Content string; At time.Time }`
  - `func readTranscriptFrom(path string, offset int64, gap time.Duration, maxBytes int) ([]segment, int64, error)` — note the two new parameters; every caller must pass them.
  - `transcriptRecord` gains `Timestamp string \`json:"timestamp,omitempty"\`` and a `time() (time.Time, bool)` accessor parsing RFC3339.

Cutting rules, applied while walking records in order:
- start a new segment when the gap between this record's timestamp and the previous record's exceeds `gap` (skip when `gap <= 0`)
- start a new segment when the current one already holds more than `maxBytes` of rendered text (skip when `maxBytes <= 0`)
- a record with no parsable timestamp inherits the previous record's, and never forces a cut
- boundaries fall between records, never inside one
- a final segment shorter than `minSegmentBytes` merges backwards into its predecessor rather than being posted alone

Add `const minSegmentBytes = 200` in hook.go with a comment: a two-line coda is not its own idea, and the extractor's own floor is far lower, so this is about not wasting a gate evaluation.

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
docker run --rm -v "$PWD":/src:ro -v /tmp/gocache:/gocache \
  -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod \
  -w /src golang:1.26 go test ./cmd/anamnesia/ -run 'TestSegment|TestRecordsWithout|TestBothSettings|TestAShortTrailing' -v
```

Expected: build failure — `readTranscriptFrom` takes 2 arguments, not 4, and `segment` is undefined.

- [ ] **Step 3: Declare the two settings**

In `cmd/anamnesia/settings.go`, after the `worker` block:

```go
	// ─── ingest ──────────────────────────────────────────────────────
	{Key: "ingest.segment_gap", Kind: kDuration, Def: "20m", Env: "",
		Doc: "A pause longer than this starts a new segment when a checkpoint is cut up, so the surprise gate judges one subject at a time rather than a whole session. Set to 0 to send each checkpoint as a single source, which is what earlier versions did."},
	{Key: "ingest.segment_max_bytes", Kind: kInt, Def: "32768", Env: "",
		Doc: "A segment is cut when it grows past this, because a long unbroken session is still not one idea. Set to 0 to disable the size cut."},
```

`Env` is empty on both: segmentation happens in the hook, which reads the host config directly. The server never sees these.

- [ ] **Step 4: Implement segmentation**

Add to `hook.go`:

```go
// segment is one piece of a checkpoint: contiguous turns that look like one
// subject, and when the first of them happened.
type segment struct {
	Content string
	At      time.Time
}

// minSegmentBytes is the shortest tail worth posting on its own. Below it a
// segment merges backwards: a two-line coda is not its own idea, and posting
// it costs a gate evaluation to learn that.
const minSegmentBytes = 200
```

Give `transcriptRecord` a timestamp:

```go
	Timestamp string `json:"timestamp,omitempty"`
```

```go
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
```

Then rewrite the render loop in `readTranscriptFrom`. Keep everything above it — the file open, the shrink check, the whole-lines-only `consumed` calculation — exactly as it is. Replace the accumulate-into-one-builder loop with one that closes a segment when a rule fires:

```go
	var (
		segs    []segment
		sb      strings.Builder
		segAt   time.Time
		prevAt  time.Time
		haveSeg bool
	)
	flush := func() {
		if body := strings.TrimSpace(sb.String()); body != "" {
			segs = append(segs, segment{Content: body, At: segAt})
		}
		sb.Reset()
		haveSeg = false
	}

	for _, line := range strings.Split(string(raw[:consumed]), "\n") {
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
				flush()
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
	flush()

	// A short tail is not its own idea. Merge it backwards rather than
	// spending a gate evaluation to discover that.
	if len(segs) > 1 && len(segs[len(segs)-1].Content) < minSegmentBytes {
		last := segs[len(segs)-1]
		segs = segs[:len(segs)-1]
		segs[len(segs)-1].Content += "\n" + last.Content
	}
	return segs, offset + int64(consumed), nil
```

Change the signature to `func readTranscriptFrom(path string, offset int64, gap time.Duration, maxBytes int) ([]segment, int64, error)`.

- [ ] **Step 5: Update the existing callers and tests**

`doCheckpoint` is the only production caller; make it compile for now by passing `hc.Dur("ingest.segment_gap")` and `hc.Int("ingest.segment_max_bytes")` and joining the segments back into one string with `"\n"` — Task 2 replaces that with the posting loop. The four existing `TestReadTranscriptFrom*` tests must be updated to the new signature and to reading `segs[0].Content`; keep every assertion they already make, since they pin the tool-noise skipping and partial-line behaviour this must not change.

- [ ] **Step 6: Run the tests and watch them pass**

Same command as Step 2, plus the whole package. Expected: all pass.

- [ ] **Step 7: Commit**

```bash
git add cmd/anamnesia/settings.go cmd/anamnesia/hook.go cmd/anamnesia/hook_test.go
git commit -m "Cut a checkpoint into segments instead of one blob"
```

---

### Task 2: Post each segment as its own source

**Files:**
- Modify: `cmd/anamnesia/hook.go`
- Test: `cmd/anamnesia/hook_test.go`

**Interfaces:**
- Consumes: `segment` and the new `readTranscriptFrom` from Task 1.
- Produces: `ingestPayload` gains `OccurredAt *time.Time \`json:"occurred_at,omitempty"\``.

`doCheckpoint` posts every segment in order, and advances the offset only after all of them are accepted. A partial failure leaves the offset where it was, so the next checkpoint re-sends the range: re-sending is safe because the gate deduplicates, and losing a segment is not.

- [ ] **Step 1: Write the failing tests**

```go
// captureIngests stands up a server that records every ingest body, and
// points a host config at it.
func captureIngests(t *testing.T) (*hostConfig, *[]map[string]any) {
	t.Helper()
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"source_id":"6f1c1f7a-0a1a-4a7a-9a7a-1a7a1a7a1a7a","queued":true}`))
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
		claudeHookInput{TranscriptPath: path, SessionID: "s1"}, "claude-session"); err != nil {
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

func TestCheckpointAdvancesTheOffsetOnlyWhenEverySegmentLands(t *testing.T) {
	// Losing a segment is worse than sending one twice: the gate deduplicates
	// a repeat, and nothing recovers a segment that was skipped.
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
		[3]string{"2026-03-02T09:41:00Z", "user", "an entirely separate subject, also discussed at length"},
	)
	if _, err := doCheckpoint(context.Background(), hc,
		claudeHookInput{TranscriptPath: path, SessionID: "s2"}, "claude-session"); err == nil {
		t.Fatal("checkpoint reported success despite a failed segment")
	}
	if off := readOffset("s2", path); off != 0 {
		t.Errorf("offset advanced to %d after a failed segment; the range must be re-sent", off)
	}
}
```

Read `testHostConfig` in `install_test.go` before using it, and check whether `hostConfig.values` is reachable from a test in this package — it is the same package, so it is. If `readOffset`'s signature differs from `readOffset(sessionID, path)`, use the real one.

- [ ] **Step 2: Run the tests and watch them fail**

```bash
docker run --rm -v "$PWD":/src:ro -v /tmp/gocache:/gocache \
  -e GOCACHE=/gocache/build -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod \
  -w /src golang:1.26 go test ./cmd/anamnesia/ -run TestCheckpoint -v
```

Expected: failure — one source posted rather than two, and no `occurred_at`.

- [ ] **Step 3: Implement the posting loop**

Add `OccurredAt *time.Time \`json:"occurred_at,omitempty"\`` to `ingestPayload`, then replace the single post in `doCheckpoint`:

```go
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
	for i, seg := range segs {
		at := seg.At
		payload := ingestPayload{
			Kind:        kind,
			Title:       title,
			ExternalRef: fmt.Sprintf("%s#%d", input.SessionID, i),
			Content:     seg.Content,
			User:        hc.User(),
			Project:     hc.Project(),
			Metadata: map[string]any{
				"session_id":  input.SessionID,
				"cwd":         input.CWD,
				"stop_reason": input.StopReason,
				"trigger":     input.Trigger,
				"byte_range":  fmt.Sprintf("%d-%d", offset, next),
				"segment":     i,
				"segments":    len(segs),
			},
		}
		if !at.IsZero() {
			payload.OccurredAt = &at
		}
		if err := httpPost(ctx, hc, "/v1/ingest", payload, nil); err != nil {
			// The offset stays where it was, so the next checkpoint
			// re-sends this range. The gate deduplicates a repeat; nothing
			// recovers a segment that was skipped.
			return fmt.Sprintf("ingested %d of %d segments", sent, len(segs)), err
		}
		sent++
	}
	if err := writeOffset(input.SessionID, input.TranscriptPath, next); err != nil {
		return fmt.Sprintf("ingested %d segments, offset not saved", sent), err
	}
	return fmt.Sprintf("ingested %d segments", sent), nil
```

`ingestPayload` needs an `ExternalRef string \`json:"external_ref,omitempty"\`` field if it does not already have one — check before adding.

- [ ] **Step 4: Run the tests and watch them pass**

Same command, then the whole package and `gofmt -s -l .` and `go vet ./...`.

- [ ] **Step 5: Commit**

```bash
git add cmd/anamnesia/hook.go cmd/anamnesia/hook_test.go
git commit -m "Post each segment as its own source"
```

---

### Task 3: Prove it end to end, and measure it

**Files:** none changed unless the run finds a defect.

This is the task the whole design exists for. A synthetic session with three clearly distinct subjects must arrive as three sources, and gating one must not skip the others.

- [ ] **Step 1: Build and start a throwaway stack**

```bash
docker run --rm -v "$PWD":/src -v /tmp/gocache:/gocache -e GOCACHE=/gocache/build \
  -e GOMODCACHE=/gocache/mod -e GOFLAGS=-mod=mod -e GOOS=darwin -e GOARCH=arm64 \
  -w /src golang:1.26 go build -o bin/anamnesia ./cmd/anamnesia

export ANAMNESIA_HOME=<scratch>/anamnesia-seg-dev
./bin/anamnesia setup --no-hooks --no-start
./bin/anamnesia config set postgres.container anamnesia-seg-pg
./bin/anamnesia config set postgres.volume anamnesia-seg-pgdata
./bin/anamnesia config set postgres.port 5438
./bin/anamnesia config set server.addr 127.0.0.1:8202
./bin/anamnesia config set openrouter.api_key <the key from ~/.anamnesia/config.toml>
./bin/anamnesia config set llm.model openai/gpt-4o-mini
./bin/anamnesia config set embed.model openai/text-embedding-3-small
./bin/anamnesia config set worker.extract_every 2s
./bin/anamnesia start
```

NEVER point `ANAMNESIA_HOME` at the default location: the real install lives there and this writes data.

- [ ] **Step 2: Run the hook against a synthetic three-subject transcript**

Write a transcript JSONL with three subjects separated by gaps over 20 minutes, each substantial (400+ characters), then:

```bash
echo '{"session_id":"seg-test","transcript_path":"<path>","cwd":"/tmp"}' \
  | ./bin/anamnesia hook session-end
```

Then confirm three sources arrived, each with its own `occurred_at` and `external_ref`:

```bash
curl -s 'localhost:8202/v1/sources?limit=10' | python3 -m json.tool
```

- [ ] **Step 3: Confirm gating one does not skip the others**

Re-run the same hook a second time with the offset reset (delete the offset file). The gate should skip the segments it already has while still evaluating each one independently — the point being that a skip verdict lands per segment rather than per session. Record what actually happens; a surprising result here is a finding, not a failure to hide.

- [ ] **Step 4: Measure the change with the eval**

The eval measures retrieval over its own fixture corpus, which this change does not touch, so it will not move — **do not expect it to**. What to record instead, from a real transcript ingested both ways:

```
settings at 0 (today's behaviour):   N sources, X operations, Y gated to zero
settings at defaults (segmented):    N sources, X operations, Y gated to zero
```

Use `/v1/stats` for the source-state breakdown. The claim to test is that the same transcript yields more operations when segmented, and that fewer of its content is lost to a whole-session skip.

- [ ] **Step 5: Tear down and report**

```bash
./bin/anamnesia stop
docker rm -f anamnesia-seg-pg
docker volume rm anamnesia-seg-pgdata
```

Report the before/after numbers. If segmentation did not increase yield on a real transcript, say so plainly — that is the outcome that matters, and the design's own "How we will know it worked" section commits to backing the change out rather than shipping it on faith.

---

## Self-Review

**Spec coverage.** Time-gap and size cutting → Task 1. Boundaries never inside a turn → Task 1, tested. Missing timestamps inherit → Task 1, tested. Short tail merges → Task 1, tested. Both settings at 0 restore today's behaviour → Task 1, tested, the reversibility guarantee. `occurred_at` from the first turn → Tasks 1 and 2, tested. `external_ref` as `<session>#<n>` → Task 2, tested. Segments post in order, offset advances only on full success → Task 2, tested. One source per segment → Task 2. Integration proof → Task 3. Settings declared in settings.go only → Task 1.

**Deliberately not in this plan:** the op budget stays at 8, unchanged — the spec is explicit that 8 per segment instead of 8 per day *is* the change, and no new setting is wanted. Nothing about retrieval, the gate's threshold, or `Stop` as a hook.

**Placeholders:** none. Two steps say "check the real signature before using it" and name the file — instruction, not placeholder.

**Type consistency:** `segment{Content, At}` is defined in Task 1 and consumed in Task 2. `readTranscriptFrom`'s new four-argument signature is introduced in Task 1 and its only production caller updated in the same task, then rewritten in Task 2. `ingestPayload.OccurredAt` is added in Task 2 and used only there.

**One risk worth naming:** Task 1 Step 5 leaves `doCheckpoint` joining segments back into one string so the tree compiles. If Task 2 were abandoned, that join would silently ship segmentation that segments nothing. Task 2 is not optional.
