// pool.go — bounded concurrency for work that is mostly waiting.
//
// Extraction spends roughly 85% of its wall time blocked on two remote
// calls (the candidate fetch and the operations completion), so draining
// a batch one source at a time leaves the process idle almost the whole
// time. This is the only knob that changes that, and it is deliberately
// small: no queue, no worker structs, no lifecycle.
package jobs

import (
	"context"
	"sync"
)

// forEachLimited calls fn once per item, with at most n calls in flight.
// fn receives each item's index so callers can write results into a
// preallocated slice instead of guarding an accumulator with a lock.
//
// A non-positive n means serial, matching the behaviour before a pool
// existed: an unset config must not mean "no workers", which would drain
// nothing at all.
func forEachLimited[T any](ctx context.Context, items []T, n int, fn func(context.Context, int, T)) {
	if n < 1 {
		n = 1
	}
	if n > len(items) {
		n = len(items)
	}
	if len(items) == 0 {
		return
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(ctx, i, item)
		}()
	}
	wg.Wait()
}
