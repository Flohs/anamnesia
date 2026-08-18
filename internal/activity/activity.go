// Package activity records what the server is doing, in memory, so an
// observer can see it.
//
// The server's only trace of its own reasoning today is a line in
// server.log: which loop ran, and nothing about what it decided. This
// package holds two things instead. Loop state, one record per worker
// loop, overwritten every tick. And a bounded ring of traces, where one
// trace is one unit of reasoning: an ingest, a retrieval, a
// consolidation pass, broken into the steps it actually went through.
//
// Nothing here is persisted. A restart empties it, which is the trade
// for costing no schema, no table and no write on the hot path.
package activity

import (
	"encoding/json"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Detail payloads are bounded. A single value is capped so one enormous
// prompt cannot dominate the ring, and a whole trace is capped so one
// pathological source cannot either.
const (
	maxDetailValue = 4 << 10
	maxTraceDetail = 32 << 10
)

// Trace is one unit of reasoning, with the steps it went through.
type Trace struct {
	Seq       int64
	ID        uuid.UUID
	Kind      string // ingest | retrieve | session-start | consolidate
	Status    string // running | ok | skipped | failed
	User      string
	Project   string
	Started   time.Time
	Ended     time.Time
	Summary   string
	Steps     []Step
	StepCount int
	// StepTimings is what a list view gets instead of Steps: enough to
	// draw one bar segment per step, and nothing else.
	StepTimings []StepTiming

	rec         *Recorder
	detailBytes int // spent against maxTraceDetail
}

// Step is one stage within a trace.
type Step struct {
	Name     string
	At       time.Time
	Duration time.Duration
	Summary  string
	Detail   map[string]any
	Err      string
}

// StepTiming is a step reduced to what a bar segment needs.
type StepTiming struct {
	Name     string
	Duration time.Duration
	// Detail carries only the keys that label a segment ("llm ·
	// gpt-4o-mini", "gate · skip"). Everything else in a step's detail
	// is exactly what a summary exists to leave out.
	Detail map[string]any
}

// LoopState is one worker loop, overwritten every tick.
type LoopState struct {
	Name         string
	Interval     time.Duration
	Running      bool
	LastStart    time.Time
	LastDuration time.Duration
	LastResult   string
	LastError    string
	Runs         int64
	Failures     int64
}

// subscriberBuffer is how many events a stream may fall behind before
// its events start being dropped. Dropping is deliberate: a browser that
// stops reading must not be able to stall the extract loop.
const subscriberBuffer = 64

// Event is one thing that happened, delivered to stream subscribers.
type Event struct {
	Kind    string // trace | step | loops | queues
	Trace   *Trace
	TraceID uuid.UUID
	Step    *Step
	Loops   []LoopState
	Queues  *QueueDepth
}

// QueueDepth is how much background work is outstanding, server-wide.
// Server-wide because that is what the snapshot reports, and a reader
// refreshing the snapshot's number with a differently scoped one would
// watch a tile alternate between two quantities.
type QueueDepth struct {
	Extract int
	Embed   int
}

// Snapshot is the recorder's whole state at one instant.
type Snapshot struct {
	Capacity      int
	Held          int
	DroppedEvents int64
	Loops         []LoopState // ordered by name
	Traces        []Trace     // newest first, summaries only
}

// Recorder holds the ring. A nil *Recorder is a working no-op recorder,
// so instrumentation call sites never need a guard.
type Recorder struct {
	mu       sync.Mutex
	capacity int
	ring     []*Trace // circular, len == capacity
	next     int      // where the next trace is written
	held     int
	seq      int64
	loops    map[string]*LoopState
	subs     map[int64]chan Event
	nextSub  int64
	dropped  int64
}

// New returns a recorder holding at most capacity traces. Capacity 0 or
// less returns nil, which is the disabled recorder.
func New(capacity int) *Recorder {
	if capacity <= 0 {
		return nil
	}
	return &Recorder{
		capacity: capacity,
		ring:     make([]*Trace, capacity),
		loops:    map[string]*LoopState{},
		subs:     map[int64]chan Event{},
	}
}

// Begin opens a trace. The returned trace is nil when the recorder is,
// and every Trace method tolerates that.
func (r *Recorder) Begin(kind, user, project string) *Trace {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	t := &Trace{
		Seq:     r.seq,
		ID:      uuid.New(),
		Kind:    kind,
		Status:  "running",
		User:    user,
		Project: project,
		Started: time.Now().UTC(),
		rec:     r,
	}
	r.ring[r.next] = t
	r.next = (r.next + 1) % r.capacity
	if r.held < r.capacity {
		r.held++
	}
	r.publish(Event{Kind: "trace", Trace: t.summary()})
	return t
}

// End closes a trace with a final status and one human sentence.
func (t *Trace) End(status, summary string) {
	if t == nil || t.rec == nil {
		return
	}
	t.rec.mu.Lock()
	defer t.rec.mu.Unlock()
	t.Status = status
	t.Summary = summary
	t.Ended = time.Now().UTC()
	t.rec.publish(Event{Kind: "trace", Trace: t.summary()})
}

// Snapshot returns the recorder's state, newest trace first. Traces come
// back as summaries without their steps, and as copies: the originals
// stay in the ring where they are still being written to.
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := Snapshot{
		Capacity:      r.capacity,
		Held:          r.held,
		DroppedEvents: r.dropped,
		Loops:         r.loopStates(),
	}
	for i := 0; i < r.held; i++ {
		idx := ((r.next-1-i)%r.capacity + r.capacity) % r.capacity
		if t := r.ring[idx]; t != nil {
			snap.Traces = append(snap.Traces, *t.summary())
		}
	}
	return snap
}

// Step appends one stage to the trace.
func (t *Trace) Step(name, summary string, detail map[string]any) {
	t.append(Step{Name: name, Summary: summary, Detail: detail})
}

// Fail appends a stage that errored. The error is the summary too: a
// failed step has nothing else worth saying about it.
func (t *Trace) Fail(name string, err error) {
	s := Step{Name: name}
	if err != nil {
		s.Err = err.Error()
		s.Summary = err.Error()
	}
	t.append(s)
}

// append timestamps a step and adds it under the recorder's lock. A
// step's duration is measured from the previous step, or from the start
// of the trace for the first one.
func (t *Trace) append(s Step) {
	if t == nil || t.rec == nil {
		return
	}
	t.rec.mu.Lock()
	defer t.rec.mu.Unlock()
	now := time.Now().UTC()
	prev := t.Started
	if n := len(t.Steps); n > 0 {
		prev = t.Steps[n-1].At
	}
	s.At = now
	s.Duration = now.Sub(prev)
	var spent int
	s.Detail, spent = bound(s.Detail, maxTraceDetail-t.detailBytes)
	t.detailBytes += spent
	t.Steps = append(t.Steps, s)
	step := s
	t.rec.publish(Event{Kind: "step", TraceID: t.ID, Step: &step})
}

// Trace returns one trace with its steps. Not found once it has aged out
// of the ring.
func (r *Recorder) Trace(id uuid.UUID) (*Trace, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.ring {
		if t != nil && t.ID == id {
			return t.clone(), true
		}
	}
	return nil, false
}

// clone copies a trace and its steps. Callers never get the trace that
// is still in the ring: the producer may be appending to it while the
// caller serialises it. Must be called with the recorder locked.
func (t *Trace) clone() *Trace {
	c := *t
	c.rec = nil
	c.StepCount = len(t.Steps)
	c.Steps = make([]Step, len(t.Steps))
	copy(c.Steps, t.Steps)
	for i, s := range c.Steps {
		if s.Detail == nil {
			continue
		}
		d := make(map[string]any, len(s.Detail))
		for k, v := range s.Detail {
			d[k] = v
		}
		c.Steps[i].Detail = d
	}
	return &c
}

// bound copies a detail map, capping any single value and stopping once
// the trace's remaining budget is spent. It copies rather than edits in
// place because the map belongs to the caller.
//
// A step whose detail is dropped is still recorded as a step. Losing the
// payload of a pathological source is acceptable; losing the fact that a
// stage ran is not, because that is the part the reader is counting on.
func bound(detail map[string]any, budget int) (map[string]any, int) {
	if len(detail) == 0 {
		return nil, 0
	}
	if budget <= 0 {
		return map[string]any{"truncated": true}, 0
	}
	out := make(map[string]any, len(detail)+1)
	spent, truncated := 0, false
	for k, v := range detail {
		if str, ok := v.(string); ok && len(str) > maxDetailValue {
			v = str[:maxDetailValue]
			truncated = true
		}
		size := valueSize(v)
		room := min(maxDetailValue, budget-spent)
		if size > room {
			// A list is shortened rather than dropped: the head of a
			// ranked list or a scope list is the part worth keeping, and
			// dropping the value outright leaves a step saying only that
			// something existed.
			shorter, shorterSize, ok := headThatFits(v, room)
			if !ok {
				truncated = true
				continue
			}
			v, size, truncated = shorter, shorterSize, true
		}
		spent += size
		out[k] = v
	}
	if truncated {
		out["truncated"] = true
	}
	return out, spent
}

// valueSize is what a detail value costs, measured the way it will be
// served: as JSON.
func valueSize(v any) int {
	switch t := v.(type) {
	case nil:
		return 0
	case string:
		return len(t)
	}
	if b, err := json.Marshal(v); err == nil {
		return len(b)
	}
	return 0
}

// SetInterval records the cadence a loop is configured for, so the
// reader can tell "has not run in a while" from "runs once a day".
func (r *Recorder) SetInterval(name string, d time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loop(name).Interval = d
}

// LoopStart marks a loop's tick as running and returns the function that
// closes it out. The result string is what the loop did in one sentence,
// which is the only thing that makes a worker lane readable.
func (r *Recorder) LoopStart(name string) func(result string, err error) {
	if r == nil {
		return func(string, error) {}
	}
	start := time.Now().UTC()
	r.mu.Lock()
	l := r.loop(name)
	l.Running = true
	l.LastStart = start
	r.publish(Event{Kind: "loops", Loops: r.loopStates()})
	r.mu.Unlock()

	return func(result string, err error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		l := r.loop(name)
		l.Running = false
		l.LastDuration = time.Since(start)
		l.LastResult = result
		l.Runs++
		if err != nil {
			l.Failures++
			l.LastError = err.Error()
		} else {
			l.LastError = ""
		}
		r.publish(Event{Kind: "loops", Loops: r.loopStates()})
	}
}

// PublishQueues announces queue depth. Called after work that can have
// changed it, rather than every tick: depth moves on exactly three
// occasions and the rest of the time this would be two COUNTs answering
// a question nobody asked.
func (r *Recorder) PublishQueues(extract, embed int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.publish(Event{Kind: "queues", Queues: &QueueDepth{Extract: extract, Embed: embed}})
}

// loop returns the named loop's state, creating it on first sight. Must
// be called with the recorder locked.
func (r *Recorder) loop(name string) *LoopState {
	l, ok := r.loops[name]
	if !ok {
		l = &LoopState{Name: name}
		r.loops[name] = l
	}
	return l
}

// summary copies a trace without its steps, for the list views and for
// the stream. The steps' names and durations survive: they are what a
// reader needs to see where the time went, and they cost a fraction of
// the steps themselves. Must be called with the recorder locked.
func (t *Trace) summary() *Trace {
	c := *t
	c.rec = nil
	c.Steps = nil
	c.StepCount = len(t.Steps)
	c.StepTimings = nil
	if len(t.Steps) > 0 {
		c.StepTimings = make([]StepTiming, len(t.Steps))
		for i, s := range t.Steps {
			c.StepTimings[i] = StepTiming{
				Name:     s.Name,
				Duration: s.Duration,
				Detail:   labelDetail(s.Detail),
			}
		}
	}
	return &c
}

// labelKeys are the detail keys a bar segment can be labelled with.
var labelKeys = [...]string{"model", "verdict"}

// labelDetail keeps the labelling keys and drops the rest, returning nil
// when there are none so the field stays absent rather than empty.
func labelDetail(d map[string]any) map[string]any {
	var out map[string]any
	for _, k := range labelKeys {
		v, ok := d[k]
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]any, len(labelKeys))
		}
		out[k] = v
	}
	return out
}

// loopStates copies the loop table, ordered by name so the worker lane
// does not reshuffle itself between polls. Must be called with the
// recorder locked.
func (r *Recorder) loopStates() []LoopState {
	out := make([]LoopState, 0, len(r.loops))
	for _, l := range r.loops {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Subscribe returns a channel of events and the function that closes it.
// The caller must call that function, or the recorder keeps publishing
// into a channel nobody reads.
func (r *Recorder) Subscribe() (<-chan Event, func()) {
	if r == nil {
		// A closed channel rather than nil: a reader ranging over it
		// finishes instead of blocking forever.
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextSub++
	id := r.nextSub
	ch := make(chan Event, subscriberBuffer)
	r.subs[id] = ch
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if c, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(c)
		}
	}
}

// publish fans an event out to subscribers, dropping it for anyone who
// has fallen a whole buffer behind. The count is reported in the
// snapshot: a feed with holes in it has to admit as much. Must be called
// with the recorder locked, which is also what makes closing a
// subscriber's channel safe.
func (r *Recorder) publish(ev Event) {
	for _, ch := range r.subs {
		select {
		case ch <- ev:
		default:
			r.dropped++
		}
	}
}

// headThatFits keeps as many leading elements of a slice as the budget
// allows. Reports false for anything that is not a slice, or a slice
// whose first element alone is already too big.
func headThatFits(v any, budget int) (any, int, bool) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Slice {
		return nil, 0, false
	}
	out := reflect.MakeSlice(rv.Type(), 0, rv.Len())
	spent := 2 // the brackets
	for i := 0; i < rv.Len(); i++ {
		size := valueSize(rv.Index(i).Interface()) + 1 // and the comma
		if spent+size > budget {
			break
		}
		out = reflect.Append(out, rv.Index(i))
		spent += size
	}
	if out.Len() == 0 {
		return nil, 0, false
	}
	return out.Interface(), spent, true
}
