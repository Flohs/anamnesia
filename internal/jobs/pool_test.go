package jobs

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestForEachLimitedRunsEveryItem(t *testing.T) {
	var mu sync.Mutex
	seen := map[int]bool{}
	items := []string{"a", "b", "c", "d", "e"}

	forEachLimited(context.Background(), items, 3, func(_ context.Context, i int, _ string) {
		mu.Lock()
		defer mu.Unlock()
		seen[i] = true
	})

	if len(seen) != len(items) {
		t.Errorf("ran %d of %d items", len(seen), len(items))
	}
}

func TestForEachLimitedNeverExceedsTheLimit(t *testing.T) {
	// Extraction is a paid API call per item. A pool that ignores its
	// bound would multiply the provider's rate-limit pressure by the
	// batch size the moment a backlog appears.
	var live, peak int64
	items := make([]int, 20)

	forEachLimited(context.Background(), items, 4, func(_ context.Context, _ int, _ int) {
		n := atomic.AddInt64(&live, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if n <= old || atomic.CompareAndSwapInt64(&peak, old, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		atomic.AddInt64(&live, -1)
	})

	if peak > 4 {
		t.Errorf("peak concurrency %d, want at most 4", peak)
	}
	if peak < 2 {
		t.Errorf("peak concurrency %d: nothing ran in parallel, so the pool is not a pool", peak)
	}
}

func TestForEachLimitedTreatsANonPositiveLimitAsSerial(t *testing.T) {
	// A zero from an unset config must not mean "no workers", which
	// would drain nothing and stall the queue forever.
	for _, n := range []int{0, -1} {
		var count int64
		forEachLimited(context.Background(), make([]int, 3), n, func(_ context.Context, _ int, _ int) {
			atomic.AddInt64(&count, 1)
		})
		if count != 3 {
			t.Errorf("limit %d ran %d of 3 items", n, count)
		}
	}
}

func TestForEachLimitedHandlesALimitAboveTheItemCount(t *testing.T) {
	var count int64
	forEachLimited(context.Background(), make([]int, 2), 16, func(_ context.Context, _ int, _ int) {
		atomic.AddInt64(&count, 1)
	})
	if count != 2 {
		t.Errorf("ran %d of 2 items", count)
	}
}

func TestForEachLimitedGivesEachCallItsOwnIndex(t *testing.T) {
	// tickExtract writes per-source results into a preallocated slice by
	// index, which is what keeps the aggregation lock-free. Duplicate or
	// skipped indices would silently drop or overwrite results.
	items := []int{10, 20, 30, 40}
	got := make([]int, len(items))
	forEachLimited(context.Background(), items, 3, func(_ context.Context, i int, item int) {
		got[i] = item
	})
	for i, want := range items {
		if got[i] != want {
			t.Errorf("index %d got %d, want %d", i, got[i], want)
		}
	}
}

func TestForEachLimitedOnAnEmptySliceDoesNothing(t *testing.T) {
	forEachLimited(context.Background(), []int{}, 4, func(_ context.Context, _ int, _ int) {
		t.Error("callback ran for an empty slice")
	})
}
