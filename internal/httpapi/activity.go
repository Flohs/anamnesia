// activity.go serves the read-only view of what the server is doing:
// the worker loops, and the traces the recorder holds.
//
// Everything here is a projection of internal/activity into the wire
// contract. The recorder keeps Go types (time.Time, time.Duration); the
// UI wants RFC3339 and milliseconds, and that translation belongs on
// this side of the boundary rather than in the recorder.
//
// With no recorder these routes are 404 rather than empty. An empty
// answer would say "nothing is happening" when the truth is "nobody is
// watching", and those are different things.
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/activity"
)

// pingEvery keeps intermediaries from closing an idle stream.
const pingEvery = 15 * time.Second

type activityResponse struct {
	Server   serverInfo   `json:"server"`
	Recorder recorderInfo `json:"recorder"`
	Loops    []loopView   `json:"loops"`
	// Queues is server-wide and needs a database, so it is absent rather
	// than zeroed when there is none: "0 pending" and "not measured" are
	// different statements.
	Queues *QueuePendingResponse `json:"queues,omitempty"`
	Traces []traceView           `json:"traces"`
}

type serverInfo struct {
	StartedAt string `json:"started_at"`
	UptimeMS  int64  `json:"uptime_ms"`
	Version   string `json:"version,omitempty"`
}

type recorderInfo struct {
	Capacity      int   `json:"capacity"`
	Held          int   `json:"held"`
	DroppedEvents int64 `json:"dropped_events"`
}

type loopView struct {
	Name           string `json:"name"`
	IntervalMS     int64  `json:"interval_ms"`
	Running        bool   `json:"running"`
	LastStart      string `json:"last_start,omitempty"`
	LastDurationMS int64  `json:"last_duration_ms"`
	LastResult     string `json:"last_result"`
	LastError      string `json:"last_error"`
	Runs           int64  `json:"runs"`
	Failures       int64  `json:"failures"`
}

type traceView struct {
	ID         string     `json:"id"`
	Seq        int64      `json:"seq"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	User       string     `json:"user,omitempty"`
	Project    string     `json:"project,omitempty"`
	StartedAt  string     `json:"started_at"`
	EndedAt    string     `json:"ended_at,omitempty"`
	DurationMS int64      `json:"duration_ms"`
	Summary    string     `json:"summary"`
	StepCount  int        `json:"step_count"`
	Steps      []stepView `json:"steps,omitempty"`
}

type stepView struct {
	Name       string         `json:"name"`
	At         string         `json:"at"`
	DurationMS int64          `json:"duration_ms"`
	Summary    string         `json:"summary,omitempty"`
	Err        string         `json:"err,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
}

// stepEvent is what the stream sends when a step lands, so the UI can
// grow a running trace in place instead of refetching it.
type stepEvent struct {
	TraceID string   `json:"trace_id"`
	Step    stepView `json:"step"`
}

func (d Deps) handleActivity(w http.ResponseWriter, r *http.Request) {
	if d.Activity == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, d.activitySnapshot(r.Context()))
}

func (d Deps) handleActivityTrace(w http.ResponseWriter, r *http.Request) {
	if d.Activity == nil {
		http.NotFound(w, r)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "id must be a uuid", http.StatusBadRequest)
		return
	}
	tr, ok := d.Activity.Trace(id)
	if !ok {
		// Aged out of the ring, or never existed. The ring is bounded
		// and in memory, so this is ordinary rather than exceptional.
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, viewTrace(*tr, true))
}

func (d Deps) handleActivityStream(w http.ResponseWriter, r *http.Request) {
	if d.Activity == nil {
		http.NotFound(w, r)
		return
	}
	rc := http.NewResponseController(w)
	// This response outlives the server's WriteTimeout by design, so the
	// deadline has to go. Failing loudly here beats a stream that dies
	// silently at the one minute mark.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		http.Error(w, "streaming is not supported by this server: "+err.Error(),
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // for any proxy in between
	w.WriteHeader(http.StatusOK)

	// Subscribe before snapshotting, so nothing that happens in between
	// falls down the gap.
	events, unsubscribe := d.Activity.Subscribe()
	defer unsubscribe()

	if err := writeSSE(w, "snapshot", d.activitySnapshot(r.Context())); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}

	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			name, payload := sseFrame(ev)
			if name == "" {
				continue
			}
			if err := writeSSE(w, name, payload); err != nil {
				return
			}
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

// activitySnapshot builds the /v1/activity body, which is also the
// stream's opening frame: one connection is then enough to render the
// whole screen.
func (d Deps) activitySnapshot(ctx context.Context) activityResponse {
	snap := d.Activity.Snapshot()
	resp := activityResponse{
		Server: serverInfo{
			StartedAt: rfc3339(d.Started),
			UptimeMS:  time.Since(d.Started).Milliseconds(),
			Version:   d.Version,
		},
		Recorder: recorderInfo{
			Capacity:      snap.Capacity,
			Held:          snap.Held,
			DroppedEvents: snap.DroppedEvents,
		},
		Loops:  make([]loopView, 0, len(snap.Loops)),
		Traces: make([]traceView, 0, len(snap.Traces)),
	}
	for _, l := range snap.Loops {
		resp.Loops = append(resp.Loops, viewLoop(l))
	}
	for _, t := range snap.Traces {
		resp.Traces = append(resp.Traces, viewTrace(t, false))
	}
	if d.Store != nil {
		if extract, embed, err := d.Store.QueuePendingAll(ctx); err == nil {
			resp.Queues = &QueuePendingResponse{ExtractPending: extract, EmbedPending: embed}
		}
	}
	return resp
}

// sseFrame turns a recorder event into an event name and a payload. An
// empty name means "nothing to send".
func sseFrame(ev activity.Event) (string, any) {
	switch ev.Kind {
	case "trace":
		if ev.Trace == nil {
			return "", nil
		}
		return "trace", viewTrace(*ev.Trace, false)
	case "step":
		if ev.Step == nil {
			return "", nil
		}
		return "step", stepEvent{TraceID: ev.TraceID.String(), Step: viewStep(*ev.Step)}
	case "loops":
		loops := make([]loopView, 0, len(ev.Loops))
		for _, l := range ev.Loops {
			loops = append(loops, viewLoop(l))
		}
		return "loops", loops
	}
	return "", nil
}

func writeSSE(w io.Writer, event string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
	return err
}

func viewLoop(l activity.LoopState) loopView {
	return loopView{
		Name:           l.Name,
		IntervalMS:     l.Interval.Milliseconds(),
		Running:        l.Running,
		LastStart:      rfc3339(l.LastStart),
		LastDurationMS: l.LastDuration.Milliseconds(),
		LastResult:     l.LastResult,
		LastError:      l.LastError,
		Runs:           l.Runs,
		Failures:       l.Failures,
	}
}

// viewTrace renders a trace. withSteps is false for list and stream
// frames, which carry step_count instead: a ring of 200 traces with
// every step inlined is a payload nobody asked for.
func viewTrace(t activity.Trace, withSteps bool) traceView {
	v := traceView{
		ID:        t.ID.String(),
		Seq:       t.Seq,
		Kind:      t.Kind,
		Status:    t.Status,
		User:      t.User,
		Project:   t.Project,
		StartedAt: rfc3339(t.Started),
		EndedAt:   rfc3339(t.Ended),
		Summary:   t.Summary,
		StepCount: t.StepCount,
	}
	if len(t.Steps) > v.StepCount {
		v.StepCount = len(t.Steps)
	}
	end := t.Ended
	if end.IsZero() {
		end = time.Now().UTC() // still running: report elapsed so far
	}
	v.DurationMS = end.Sub(t.Started).Milliseconds()
	if withSteps {
		v.Steps = make([]stepView, 0, len(t.Steps))
		for _, s := range t.Steps {
			v.Steps = append(v.Steps, viewStep(s))
		}
	}
	return v
}

func viewStep(s activity.Step) stepView {
	return stepView{
		Name:       s.Name,
		At:         rfc3339(s.At),
		DurationMS: s.Duration.Milliseconds(),
		Summary:    s.Summary,
		Err:        s.Err,
		Detail:     s.Detail,
	}
}

// rfc3339 renders a timestamp in UTC, and an absent one as absent.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// humanBytes renders a size the way a sentence would.
func humanBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f kB", float64(n)/1024)
}
