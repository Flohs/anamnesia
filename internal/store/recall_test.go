package store

import (
	"context"
	"testing"
)

func TestRecordRetrievalCountsEachOutcomeAgainstTheSameDay(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	for _, out := range []RecallOutcome{RecallHit, RecallHit, RecallMiss, RecallUngraded} {
		if err := st.RecordRetrieval(ctx, scope.UserID, out); err != nil {
			t.Fatalf("record %s: %v", out, err)
		}
	}

	got, err := st.RecallWindow(ctx, scope.UserID, 7)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	want := RecallCounts{Days: 7, Prompts: 4, Recalled: 2, Ungraded: 1}
	if got != want {
		t.Errorf("after two hits, one miss and one ungraded: got %+v, want %+v", got, want)
	}
}

func TestRecallWindowLeavesOutDaysBeforeIt(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	if err := st.RecordRetrieval(ctx, scope.UserID, RecallHit); err != nil {
		t.Fatal(err)
	}
	// A day old enough to fall outside the window. Written directly
	// because RecordRetrieval only ever writes today.
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO recall_daily (user_id, day, prompts, recalled, ungraded)
		VALUES ($1, (now() AT TIME ZONE 'utc')::date - 30, 99, 99, 0)`,
		scope.UserID); err != nil {
		t.Fatalf("seed old day: %v", err)
	}

	got, err := st.RecallWindow(ctx, scope.UserID, 7)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if got.Prompts != 1 || got.Recalled != 1 {
		t.Errorf("a 30-day-old row leaked into a 7-day window: got %+v", got)
	}
}

func TestRecallWindowCountsOnlyTheUserAskedFor(t *testing.T) {
	st, scope := testStore(t)
	ctx := context.Background()

	other, err := st.EnsureUser(ctx, "recall-other-"+scope.UserID.String()[:8])
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := st.RecordRetrieval(ctx, other, RecallHit); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordRetrieval(ctx, scope.UserID, RecallMiss); err != nil {
		t.Fatal(err)
	}

	got, err := st.RecallWindow(ctx, scope.UserID, 7)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if got.Prompts != 1 || got.Recalled != 0 {
		t.Errorf("another user's retrievals were counted: got %+v", got)
	}
}

func TestRecallWindowIsEmptyBeforeAnythingIsRecorded(t *testing.T) {
	st, scope := testStore(t)

	got, err := st.RecallWindow(context.Background(), scope.UserID, 7)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if want := (RecallCounts{Days: 7}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
