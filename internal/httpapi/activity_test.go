package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flohs/anamnesia/internal/activity"
)

// testServer wires just enough of the API to exercise the read routes.
// There is no store: these routes must degrade rather than panic when
// the database is not part of the test.
func testServer(t *testing.T, d Deps) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewServer("", d).Handler)
	t.Cleanup(srv.Close)
	return srv
}

func getJSON(t *testing.T, url string, into any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return resp.StatusCode
}

func TestActivityReportsLoopsAndTraces(t *testing.T) {
	rec := activity.New(10)
	rec.SetInterval("extract", 15_000_000_000) // 15s
	rec.LoopStart("extract")("1 source, 2 operations", nil)
	tr := rec.Begin("ingest", "alice", "anamnesia-ui")
	tr.Step("source", "Received a 4.2 kB chat-turn checkpoint", map[string]any{"bytes": 4200})
	tr.End("ok", "Extracted 2 operations")

	srv := testServer(t, Deps{Activity: rec, Version: "v0.1.0-rc3"})

	var got map[string]any
	if code := getJSON(t, srv.URL+"/v1/activity", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	server := got["server"].(map[string]any)
	if server["version"] != "v0.1.0-rc3" {
		t.Errorf("server.version = %v", server["version"])
	}
	recorder := got["recorder"].(map[string]any)
	if recorder["capacity"] != float64(10) || recorder["held"] != float64(1) {
		t.Errorf("recorder = %v, want capacity 10 held 1", recorder)
	}
	loops := got["loops"].([]any)
	if len(loops) != 1 {
		t.Fatalf("loops = %v, want one", loops)
	}
	loop := loops[0].(map[string]any)
	if loop["name"] != "extract" || loop["interval_ms"] != float64(15000) ||
		loop["last_result"] != "1 source, 2 operations" || loop["runs"] != float64(1) {
		t.Errorf("loop = %v", loop)
	}
	traces := got["traces"].([]any)
	if len(traces) != 1 {
		t.Fatalf("traces = %v, want one", traces)
	}
	trace := traces[0].(map[string]any)
	if trace["kind"] != "ingest" || trace["status"] != "ok" ||
		trace["project"] != "anamnesia-ui" || trace["step_count"] != float64(1) {
		t.Errorf("trace = %v", trace)
	}
	if _, ok := trace["steps"]; ok {
		t.Error("the list view carries step_count, not the steps themselves")
	}
	if _, ok := got["queues"]; ok {
		t.Error("queue counts need a database, and this server has none")
	}
}

func TestActivityTraceDetailCarriesSteps(t *testing.T) {
	rec := activity.New(10)
	tr := rec.Begin("ingest", "alice", "proj")
	tr.Step("gate", "Kept: no similar memory", map[string]any{"verdict": "keep"})
	tr.End("ok", "done")
	srv := testServer(t, Deps{Activity: rec})

	var got map[string]any
	if code := getJSON(t, srv.URL+"/v1/activity/"+tr.ID.String(), &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	steps := got["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("steps = %v, want one", steps)
	}
	step := steps[0].(map[string]any)
	if step["name"] != "gate" || step["summary"] != "Kept: no similar memory" {
		t.Errorf("step = %v", step)
	}
	if detail := step["detail"].(map[string]any); detail["verdict"] != "keep" {
		t.Errorf("detail = %v, want the verdict carried through", detail)
	}
}

func TestActivityTraceThatAgedOutIs404(t *testing.T) {
	rec := activity.New(1)
	first := rec.Begin("ingest", "alice", "proj")
	first.End("ok", "first")
	rec.Begin("ingest", "alice", "proj").End("ok", "second") // evicts the first

	srv := testServer(t, Deps{Activity: rec})
	if code := getJSON(t, srv.URL+"/v1/activity/"+first.ID.String(), nil); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a trace that aged out of the ring", code)
	}
}

func TestActivityRoutesAre404WhenRecordingIsOff(t *testing.T) {
	srv := testServer(t, Deps{Activity: nil})
	for _, path := range []string{
		"/v1/activity",
		"/v1/activity/8d4e4ec4-1f7f-4a6a-9d5e-2b6d1f0d2f11",
		"/v1/activity/stream",
	} {
		if code := getJSON(t, srv.URL+path, nil); code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 when activity.enabled is false", path, code)
		}
	}
}

// sseReader reads one server-sent event, failing the test rather than
// hanging when nothing arrives. A stream that never flushes looks
// exactly like a slow one, which is why this has a deadline.
type sseReader struct {
	t *testing.T
	r *bufio.Reader
}

func (s *sseReader) frame() (event string, data []byte) {
	s.t.Helper()
	type result struct {
		event string
		data  []byte
	}
	got := make(chan result, 1)
	go func() {
		var ev string
		var payload []byte
		for {
			line, err := s.r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event:"):
				ev = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				payload = append(payload, strings.TrimSpace(strings.TrimPrefix(line, "data:"))...)
			case line == "" && (ev != "" || len(payload) > 0):
				got <- result{ev, payload}
				return
			}
		}
	}()
	select {
	case r := <-got:
		return r.event, r.data
	case <-time.After(5 * time.Second):
		s.t.Fatal("no event arrived: the stream is buffering rather than flushing")
		return "", nil
	}
}

func TestActivityStreamSendsSnapshotThenLiveEvents(t *testing.T) {
	rec := activity.New(10)
	srv := testServer(t, Deps{Activity: rec, Version: "v0.1.0-rc3"})

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/activity/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	stream := &sseReader{t: t, r: bufio.NewReader(resp.Body)}

	event, data := stream.frame()
	if event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot so one connection renders the screen", event)
	}
	var snap map[string]any
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("snapshot payload: %v", err)
	}
	for _, key := range []string{"server", "recorder", "loops", "traces"} {
		if _, ok := snap[key]; !ok {
			t.Errorf("snapshot frame is missing %q; it must carry the whole /v1/activity body", key)
		}
	}

	// Something happens while the stream is open.
	rec.Begin("ingest", "alice", "proj").End("ok", "Extracted 2 operations")

	event, data = stream.frame()
	if event != "trace" {
		t.Fatalf("second event = %q, want trace", event)
	}
	var trace map[string]any
	if err := json.Unmarshal(data, &trace); err != nil {
		t.Fatalf("trace payload: %v", err)
	}
	if trace["kind"] != "ingest" {
		t.Errorf("streamed trace = %v, want the ingest that just ran", trace)
	}
}

func TestActivityListCarriesStepTimings(t *testing.T) {
	rec := activity.New(10)
	tr := rec.Begin("retrieve", "alice", "proj")
	tr.Step("query", "Retrieving for \"hooks\"", map[string]any{"text": "hooks"})
	tr.Step("gate", "Kept: nothing similar", map[string]any{"verdict": "keep", "reason": "novel"})
	tr.Step("llm", "returned 2 operations", map[string]any{
		"model": "gpt-4o-mini", "raw_response": "…"})
	tr.End("ok", "Extracted 2 operations")

	srv := testServer(t, Deps{Activity: rec})
	var got map[string]any
	if code := getJSON(t, srv.URL+"/v1/activity", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	trace := got["traces"].([]any)[0].(map[string]any)
	if _, ok := trace["steps"]; ok {
		t.Error("the list view still carries step_count and timings, not the steps themselves")
	}
	raw, ok := trace["step_timings"].([]any)
	if !ok {
		t.Fatalf("trace = %v, want step_timings so the console can size a segmented bar", trace)
	}
	if len(raw) != 3 {
		t.Fatalf("step_timings = %v, want one per step", raw)
	}
	for i, name := range []string{"query", "gate", "llm"} {
		timing := raw[i].(map[string]any)
		if timing["name"] != name {
			t.Errorf("timing %d name = %v, want %q in execution order", i, timing["name"], name)
		}
		if _, ok := timing["duration_ms"].(float64); !ok {
			t.Errorf("timing %d = %v, want a duration_ms", i, timing)
		}
		if _, ok := timing["summary"]; ok {
			t.Errorf("timing %d = %v, a bar segment needs no summary", i, timing)
		}
	}
	if _, ok := raw[0].(map[string]any)["detail"]; ok {
		t.Error("the query timing carries no labelling detail and must omit the field")
	}
	gate := raw[1].(map[string]any)["detail"].(map[string]any)
	if len(gate) != 1 || gate["verdict"] != "keep" {
		t.Errorf("gate detail = %v, want the verdict alone", gate)
	}
	llm := raw[2].(map[string]any)["detail"].(map[string]any)
	if len(llm) != 1 || llm["model"] != "gpt-4o-mini" {
		t.Errorf("llm detail = %v, want the model alone", llm)
	}
}

func TestStreamedTraceFrameCarriesStepTimings(t *testing.T) {
	rec := activity.New(10)
	events, unsubscribe := rec.Subscribe()
	defer unsubscribe()

	tr := rec.Begin("retrieve", "alice", "proj")
	tr.Step("vector", "12 vector hits", nil)
	tr.End("ok", "Returned 12 hits")

	// The console renders the feed from whichever frame arrives first, so
	// the closing trace frame has to carry what the list view carries.
	var closed *activity.Trace
	for len(events) > 0 {
		ev := <-events
		if ev.Kind == "trace" && ev.Trace != nil && ev.Trace.Status == "ok" {
			closed = ev.Trace
		}
	}
	if closed == nil {
		t.Fatal("no closing trace event was published")
	}
	name, payload := sseFrame(activity.Event{Kind: "trace", Trace: closed})
	if name != "trace" {
		t.Fatalf("frame = %q, want trace", name)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err := json.Unmarshal(body, &frame); err != nil {
		t.Fatal(err)
	}
	timings, ok := frame["step_timings"].([]any)
	if !ok || len(timings) != 1 {
		t.Fatalf("frame = %v, want one step_timings entry", frame)
	}
	if timings[0].(map[string]any)["name"] != "vector" {
		t.Errorf("timing = %v, want the vector step", timings[0])
	}
}
