package activity

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRingEvictsOldestAtCapacity(t *testing.T) {
	r := New(2)
	for _, summary := range []string{"first", "second", "third"} {
		tr := r.Begin("ingest", "alice", "proj")
		tr.End("ok", summary)
	}

	snap := r.Snapshot()
	if snap.Capacity != 2 {
		t.Errorf("capacity = %d, want 2", snap.Capacity)
	}
	if snap.Held != 2 {
		t.Fatalf("held = %d, want 2", snap.Held)
	}
	if snap.Traces[0].Summary != "third" || snap.Traces[1].Summary != "second" {
		t.Errorf("traces = %q, %q; want newest first: \"third\", \"second\"",
			snap.Traces[0].Summary, snap.Traces[1].Summary)
	}
}

func TestStepsAreFetchableByTraceID(t *testing.T) {
	r := New(4)
	tr := r.Begin("ingest", "alice", "proj")
	tr.Step("source", "Received a checkpoint", map[string]any{"bytes": 42})
	tr.Fail("llm", errors.New("model refused"))
	tr.End("failed", "extraction failed")

	got, ok := r.Trace(tr.ID)
	if !ok {
		t.Fatalf("trace %s not found", tr.ID)
	}
	if got.Status != "failed" {
		t.Errorf("status = %q, want %q", got.Status, "failed")
	}
	if len(got.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(got.Steps))
	}
	if got.Steps[0].Name != "source" || got.Steps[0].Detail["bytes"] != 42 {
		t.Errorf("step 0 = %+v, want name=source detail[bytes]=42", got.Steps[0])
	}
	if got.Steps[0].At.IsZero() {
		t.Error("step 0 has no timestamp")
	}
	if got.Steps[1].Name != "llm" || got.Steps[1].Err != "model refused" {
		t.Errorf("step 1 = %+v, want name=llm err=%q", got.Steps[1], "model refused")
	}
}

func TestUnknownTraceIDIsNotFound(t *testing.T) {
	r := New(4)
	r.Begin("ingest", "alice", "proj").End("ok", "done")
	if _, ok := r.Trace(uuid.New()); ok {
		t.Error("Trace reported a random id as found")
	}
}

func TestTraceReturnsACopyNotTheLiveTrace(t *testing.T) {
	r := New(4)
	tr := r.Begin("ingest", "alice", "proj")
	tr.Step("source", "received", map[string]any{"bytes": 1})

	got, ok := r.Trace(tr.ID)
	if !ok {
		t.Fatal("trace not found")
	}
	// The producer carries on after the caller has its copy.
	tr.Step("gate", "kept", nil)
	tr.End("ok", "done")

	if len(got.Steps) != 1 {
		t.Errorf("copy grew to %d steps: the caller was handed the live trace", len(got.Steps))
	}
	if got.Status != "running" {
		t.Errorf("copy status = %q, want %q as it was at the time of the call", got.Status, "running")
	}
}

func TestOversizedDetailValueIsTruncated(t *testing.T) {
	r := New(2)
	tr := r.Begin("ingest", "alice", "proj")
	tr.Step("source", "received", map[string]any{
		"excerpt": strings.Repeat("x", maxDetailValue+500),
		"bytes":   12,
	})
	tr.End("ok", "done")

	got, _ := r.Trace(tr.ID)
	d := got.Steps[0].Detail
	excerpt, ok := d["excerpt"].(string)
	if !ok {
		t.Fatalf("excerpt = %T, want string", d["excerpt"])
	}
	if len(excerpt) > maxDetailValue {
		t.Errorf("excerpt kept %d bytes, want at most %d", len(excerpt), maxDetailValue)
	}
	if d["truncated"] != true {
		t.Error("truncation was not flagged in the detail")
	}
	if d["bytes"] != 12 {
		t.Errorf("bytes = %v, want the small value left alone", d["bytes"])
	}
}

func TestTraceDetailIsBoundedInTotal(t *testing.T) {
	r := New(2)
	tr := r.Begin("ingest", "alice", "proj")
	chunk := strings.Repeat("y", maxDetailValue)
	for i := 0; i < 20; i++ { // 80 KB offered against a 32 KB budget
		tr.Step("chunk", "a big step", map[string]any{"blob": chunk})
	}
	tr.End("ok", "done")

	got, _ := r.Trace(tr.ID)
	if len(got.Steps) != 20 {
		t.Errorf("steps = %d, want 20: detail is dropped, steps are not", len(got.Steps))
	}
	total := 0
	for _, s := range got.Steps {
		for _, v := range s.Detail {
			if str, ok := v.(string); ok {
				total += len(str)
			}
		}
	}
	if total > maxTraceDetail {
		t.Errorf("trace holds %d bytes of detail, want at most %d", total, maxTraceDetail)
	}
}

func TestLoopStateRecordsRunsAndFailures(t *testing.T) {
	r := New(4)
	r.SetInterval("extract", 15*time.Second)

	r.LoopStart("extract")("1 source, 2 operations", nil)
	r.LoopStart("extract")("", errors.New("postgres is down"))

	loops := r.Snapshot().Loops
	if len(loops) != 1 {
		t.Fatalf("loops = %d, want 1", len(loops))
	}
	l := loops[0]
	if l.Name != "extract" || l.Interval != 15*time.Second {
		t.Errorf("loop = %s every %s, want extract every 15s", l.Name, l.Interval)
	}
	if l.Runs != 2 || l.Failures != 1 {
		t.Errorf("runs = %d, failures = %d; want 2 and 1", l.Runs, l.Failures)
	}
	if l.LastError != "postgres is down" {
		t.Errorf("last error = %q, want %q", l.LastError, "postgres is down")
	}
	if l.LastStart.IsZero() {
		t.Error("last start not recorded")
	}
	if l.Running {
		t.Error("loop still marked running after its tick returned")
	}
}

func TestLoopIsMarkedRunningDuringItsTick(t *testing.T) {
	r := New(4)
	done := r.LoopStart("embed")

	loops := r.Snapshot().Loops
	if len(loops) != 1 || !loops[0].Running {
		t.Fatalf("loops = %+v, want embed marked running mid-tick", loops)
	}

	done("embedded 12 facts", nil)
	if l := r.Snapshot().Loops[0]; l.Running || l.LastResult != "embedded 12 facts" {
		t.Errorf("after the tick: running = %v, result = %q", l.Running, l.LastResult)
	}
}

func TestLoopsAreOrderedByName(t *testing.T) {
	r := New(4)
	for _, name := range []string{"purge-sources", "extract", "decay"} {
		r.LoopStart(name)("ok", nil)
	}
	loops := r.Snapshot().Loops
	got := []string{loops[0].Name, loops[1].Name, loops[2].Name}
	want := []string{"decay", "extract", "purge-sources"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("loops = %v, want %v", got, want)
		}
	}
}

func TestSubscriberSeesTraceAndStepEvents(t *testing.T) {
	r := New(4)
	ch, cancel := r.Subscribe()
	defer cancel()

	tr := r.Begin("retrieve", "alice", "proj")
	tr.Step("query", "Retrieving for 'how do the hooks work'", nil)
	tr.End("ok", "Returned 5 hits")

	var got []Event
	for len(got) < 3 {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-time.After(2 * time.Second):
			t.Fatalf("channel went quiet after %d events", len(got))
		}
	}
	kinds := []string{got[0].Kind, got[1].Kind, got[2].Kind}
	want := []string{"trace", "step", "trace"}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event kinds = %v, want %v", kinds, want)
		}
	}
	if got[1].Step == nil || got[1].Step.Name != "query" || got[1].TraceID != tr.ID {
		t.Errorf("step event = %+v, want the query step of trace %s", got[1], tr.ID)
	}
	if got[2].Trace == nil || got[2].Trace.Status != "ok" {
		t.Errorf("final event = %+v, want the finished trace", got[2].Trace)
	}
}

func TestSlowSubscriberDropsEventsRatherThanBlocking(t *testing.T) {
	r := New(500)
	_, cancel := r.Subscribe() // deliberately never read
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < subscriberBuffer*3; i++ {
			r.Begin("ingest", "alice", "proj").End("ok", "done")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("producer blocked on a subscriber that never reads")
	}
	if r.Snapshot().DroppedEvents == 0 {
		t.Error("dropped events = 0: the overflow has to be counted, not hidden")
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	r := New(4)
	ch, cancel := r.Subscribe()
	cancel()

	r.Begin("ingest", "alice", "proj").End("ok", "done")

	select {
	case ev, ok := <-ch:
		if ok {
			t.Errorf("received %+v after unsubscribing", ev)
		}
	case <-time.After(time.Second):
		t.Error("channel neither closed nor quiet after unsubscribing")
	}
}

func TestNilRecorderNeverPanics(t *testing.T) {
	r := New(0)
	if r != nil {
		t.Fatal("New(0) must return nil, which is how the recorder is disabled")
	}

	r.SetInterval("extract", time.Second)
	r.LoopStart("extract")("nothing to do", nil)

	tr := r.Begin("ingest", "alice", "proj")
	if tr != nil {
		t.Error("Begin on a nil recorder must return a nil trace")
	}
	tr.Step("gate", "kept", map[string]any{"score": 0.4})
	tr.Fail("llm", errors.New("boom"))
	tr.End("ok", "done")

	if snap := r.Snapshot(); snap.Held != 0 || snap.Capacity != 0 || len(snap.Loops) != 0 {
		t.Errorf("snapshot of a nil recorder = %+v, want the zero value", snap)
	}
	if _, ok := r.Trace(uuid.New()); ok {
		t.Error("Trace on a nil recorder reported a hit")
	}
	ch, cancel := r.Subscribe()
	cancel()
	if _, ok := <-ch; ok {
		t.Error("Subscribe on a nil recorder delivered an event")
	}
}

func TestOversizedListKeepsAsManyEntriesAsFit(t *testing.T) {
	// A ranked list of candidates, or the scopes a consolidation pass
	// covered, can pass the per-value cap. Dropping the whole value
	// leaves a step that says a list existed and nothing about it, which
	// is worse than a short list: the first entries are the ones that
	// matter in every list this records.
	r := New(2)
	tr := r.Begin("retrieve", "alice", "proj")
	hits := make([]map[string]any, 300)
	for i := range hits {
		hits[i] = map[string]any{
			"id":    fmt.Sprintf("hit-%03d", i),
			"title": "a memory with a title of some length",
			"score": 0.5,
		}
	}
	tr.Step("fuse", "RRF fused 300 candidates", map[string]any{"ranked": hits})
	tr.End("ok", "done")

	got, _ := r.Trace(tr.ID)
	kept, ok := got.Steps[0].Detail["ranked"].([]map[string]any)
	if !ok {
		t.Fatalf("ranked = %T, want the list to survive in part", got.Steps[0].Detail["ranked"])
	}
	if len(kept) == 0 {
		t.Fatal("the whole list was dropped")
	}
	if len(kept) == len(hits) {
		t.Fatal("nothing was dropped, so the cap did not apply")
	}
	if kept[0]["id"] != "hit-000" {
		t.Errorf("first kept entry = %v, want the head of the list", kept[0])
	}
	if got.Steps[0].Detail["truncated"] != true {
		t.Error("a shortened list has to say so")
	}
	if size := valueSize(kept); size > maxDetailValue {
		t.Errorf("kept %d bytes, want at most %d", size, maxDetailValue)
	}
}

func TestSummaryCarriesStepTimings(t *testing.T) {
	r := New(4)
	tr := r.Begin("retrieve", "alice", "proj")
	tr.Step("query", "Retrieving for \"hooks\"", map[string]any{"text": "hooks"})
	tr.Step("gate", "Kept: nothing similar", map[string]any{"verdict": "keep", "reason": "novel"})
	tr.Step("llm", "returned 2 operations", map[string]any{
		"model": "gpt-4o-mini", "raw_response": strings.Repeat("x", 200)})
	tr.End("ok", "Extracted 2 operations")

	full, ok := r.Trace(tr.ID)
	if !ok {
		t.Fatal("trace not found")
	}
	got := r.Snapshot().Traces[0]
	if got.Steps != nil {
		t.Error("a summary must not carry its steps")
	}
	if len(got.StepTimings) != len(full.Steps) {
		t.Fatalf("step timings = %d, want one per step (%d)", len(got.StepTimings), len(full.Steps))
	}
	for i, want := range full.Steps {
		if got.StepTimings[i].Name != want.Name {
			t.Errorf("timing %d name = %q, want %q in execution order",
				i, got.StepTimings[i].Name, want.Name)
		}
		if got.StepTimings[i].Duration != want.Duration {
			t.Errorf("timing %d duration = %v, want the step's own %v",
				i, got.StepTimings[i].Duration, want.Duration)
		}
	}
}

func TestStepTimingsKeepOnlyTheLabellingDetail(t *testing.T) {
	r := New(4)
	tr := r.Begin("ingest", "alice", "proj")
	tr.Step("query", "Retrieving", map[string]any{"text": "hooks"})
	tr.Step("gate", "Kept", map[string]any{"verdict": "keep", "reason": "novel"})
	tr.Step("llm", "returned 2 operations", map[string]any{
		"model": "gpt-4o-mini", "raw_response": strings.Repeat("x", 200)})
	tr.End("ok", "done")

	timings := r.Snapshot().Traces[0].StepTimings
	if len(timings) != 3 {
		t.Fatalf("timings = %v, want three", timings)
	}
	if timings[0].Detail != nil {
		t.Errorf("query timing detail = %v, want none: it carries nothing that labels a segment",
			timings[0].Detail)
	}
	if len(timings[1].Detail) != 1 || timings[1].Detail["verdict"] != "keep" {
		t.Errorf("gate timing detail = %v, want the verdict alone", timings[1].Detail)
	}
	if len(timings[2].Detail) != 1 || timings[2].Detail["model"] != "gpt-4o-mini" {
		t.Errorf("llm timing detail = %v, want the model alone", timings[2].Detail)
	}
}

func TestQueueDepthReachesSubscribers(t *testing.T) {
	// Depth arrives once in the snapshot and then only when it changes.
	// Without an event for it, a console tile shows the depth at connect
	// time for as long as the page stays open.
	r := New(4)
	events, unsubscribe := r.Subscribe()
	defer unsubscribe()

	r.PublishQueues(2, 7)

	select {
	case ev := <-events:
		if ev.Kind != "queues" {
			t.Fatalf("event kind = %q, want queues", ev.Kind)
		}
		if ev.Queues == nil {
			t.Fatal("queues event carries no depth")
		}
		if ev.Queues.Extract != 2 || ev.Queues.Embed != 7 {
			t.Errorf("depth = %+v, want extract 2 embed 7", *ev.Queues)
		}
	default:
		t.Fatal("no event was published")
	}
}
