package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/flohs/anamnesia/internal/activity"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A cancelled context still lets the loop fire once, which is what makes
// a single tick observable without waiting for a ticker.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestLoopRecordsTheResultSentence(t *testing.T) {
	rec := activity.New(4)
	w := &Worker{Log: discardLog(), Activity: rec}

	w.loop(cancelledCtx(), "embed", time.Hour, func(context.Context) (string, error) {
		return "embedded 12 facts", nil
	})

	loops := rec.Snapshot().Loops
	if len(loops) != 1 {
		t.Fatalf("loops = %+v, want one", loops)
	}
	l := loops[0]
	if l.Name != "embed" || l.Interval != time.Hour {
		t.Errorf("loop = %s every %s, want embed every 1h", l.Name, l.Interval)
	}
	if l.Runs != 1 || l.Failures != 0 {
		t.Errorf("runs = %d, failures = %d; want 1 and 0", l.Runs, l.Failures)
	}
	if l.LastResult != "embedded 12 facts" {
		t.Errorf("last result = %q, want the tick's own sentence", l.LastResult)
	}
}

func TestLoopRecordsAFailedTick(t *testing.T) {
	rec := activity.New(4)
	w := &Worker{Log: discardLog(), Activity: rec}

	w.loop(cancelledCtx(), "extract", time.Minute, func(context.Context) (string, error) {
		return "", errors.New("postgres is down")
	})

	l := rec.Snapshot().Loops[0]
	if l.Failures != 1 || l.LastError != "postgres is down" {
		t.Errorf("failures = %d, last error = %q; want 1 and the error text", l.Failures, l.LastError)
	}
}

func TestLoopRunsWithoutARecorder(t *testing.T) {
	// Recording is optional: the worker must not depend on it.
	w := &Worker{Log: discardLog()}
	ran := false
	w.loop(cancelledCtx(), "decay", time.Hour, func(context.Context) (string, error) {
		ran = true
		return "nothing to do", nil
	})
	if !ran {
		t.Error("the tick did not run without a recorder")
	}
}
