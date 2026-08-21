package main

import (
	"testing"
	"time"
)

// TestNothingNewIsNeverFlushed. Stop fires after every assistant turn,
// including turns that added nothing to the transcript worth cutting.
func TestNothingNewIsNeverFlushed(t *testing.T) {
	if shouldFlush(0, time.Hour, 16384, 15*time.Minute) {
		t.Error("a flush was proposed with no unread bytes")
	}
	if shouldFlush(-20, time.Hour, 16384, 15*time.Minute) {
		t.Error("a flush was proposed for a truncated transcript")
	}
}

// TestASmallRecentChangeIsNotFlushed is what keeps this from becoming
// the Stop hook that was removed. Flushing every turn would cut segments
// at turn boundaries rather than topic boundaries, and pay a model call
// for each sliver.
func TestASmallRecentChangeIsNotFlushed(t *testing.T) {
	if shouldFlush(500, 30*time.Second, 16384, 15*time.Minute) {
		t.Error("500 bytes written 30 seconds ago triggered a flush")
	}
}

// TestEnoughBytesFlush: the byte gate is the primary one, because it is
// the one that lines up with segments. Reaching it means there is at
// least a segment's worth of new material to cut.
func TestEnoughBytesFlush(t *testing.T) {
	if !shouldFlush(20000, time.Second, 16384, 15*time.Minute) {
		t.Error("20000 unread bytes did not flush against a 16384 threshold")
	}
}

// TestEnoughTimeFlushes is the backstop for a slow session: a long
// conversation that never accumulates bytes quickly still should not sit
// uncheckpointed for hours.
func TestEnoughTimeFlushes(t *testing.T) {
	if !shouldFlush(800, 20*time.Minute, 16384, 15*time.Minute) {
		t.Error("a session idle past the interval did not flush")
	}
}

// TestEitherThresholdCanBeDisabled. Zero means off for each gate
// independently, and zero for both means the hook never flushes, which
// is how someone returns to checkpoint-at-the-end behaviour without
// uninstalling anything.
func TestEitherThresholdCanBeDisabled(t *testing.T) {
	if shouldFlush(999999, time.Hour, 0, 0) {
		t.Error("both thresholds off still flushed")
	}
	if !shouldFlush(999999, time.Second, 16384, 0) {
		t.Error("the byte gate stopped working when the time gate was off")
	}
	if !shouldFlush(10, time.Hour, 0, 15*time.Minute) {
		t.Error("the time gate stopped working when the byte gate was off")
	}
}

// TestStopIsInstalledForFlushing. Stop was removed once, for a reason
// that no longer holds: it re-sent the whole transcript every turn, which
// made ingest quadratic. Checkpoints have been incremental since, so a
// gated flush sends each byte once.
func TestStopIsInstalledForFlushing(t *testing.T) {
	var found bool
	for _, h := range anamnesiaHooks {
		if h.event == "Stop" {
			found = true
			if h.verb != "flush" {
				t.Errorf("Stop runs verb %q, want flush", h.verb)
			}
		}
	}
	if !found {
		t.Error("Stop is not installed, so mid-session work waits for the session to end")
	}
	if _, ok := hookTimeouts["flush"]; !ok {
		t.Error("flush has no timeout budget")
	}
}
