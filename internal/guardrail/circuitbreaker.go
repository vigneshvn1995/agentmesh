// Package guardrail — see normalizer.go for the package doc.
package guardrail

import (
	"sync"
	"time"
)

// Breaker is a concurrency-safe sliding-window circuit breaker.
//
// It tracks the timestamps of recent identical prompts (identified by a
// composite key: tenantID|agentID|normalizedHash) and blocks a request when
// the number of occurrences within the configured window reaches the limit.
//
// # Memory hygiene
//
// In-place slice filtering (times[valid] = t) is used throughout so that
// pruning expired entries never allocates a new slice. Sweep() must be called
// periodically (e.g. via a time.Ticker) to reclaim memory for keys whose
// entire history has expired.
type Breaker struct {
	mu      sync.Mutex
	history map[string][]time.Time

	window  time.Duration
	limit   int
	timeNow func() time.Time // injectable for deterministic unit tests
}

// NewBreaker constructs a Breaker that blocks requests when the same composite
// key appears more than limit times within window.
func NewBreaker(window time.Duration, limit int) *Breaker {
	return &Breaker{
		history: make(map[string][]time.Time),
		window:  window,
		limit:   limit,
		timeNow: time.Now,
	}
}

// Check reports whether the request identified by tenantID, agentID, and the
// normalised-prompt hash should be blocked.
//
// Rules:
//   - If hash is empty, Check returns false immediately and records nothing.
//     Empty strings are not meaningful deduplication keys.
//   - Expired timestamps are pruned in-place before the limit is evaluated.
//   - If the current count (after pruning) is >= limit the request is blocked
//     and the timestamp is NOT appended (the window is already exhausted).
//   - Otherwise the current timestamp is appended and false is returned.
func (b *Breaker) Check(tenantID, agentID, hash string) bool {
	if hash == "" {
		return false
	}

	key := tenantID + "|" + agentID + "|" + hash
	now := b.timeNow()
	cutoff := now.Add(-b.window)

	b.mu.Lock()
	defer b.mu.Unlock()

	times := b.history[key]

	// Prune expired entries in-place — no allocation.
	valid := 0
	for _, t := range times {
		if t.After(cutoff) {
			times[valid] = t
			valid++
		}
	}
	times = times[:valid]

	if valid >= b.limit {
		// Write the pruned slice back so we never retain stale references.
		b.history[key] = times
		return true
	}

	b.history[key] = append(times, now)
	return false
}

// Sweep iterates every tracked key and removes timestamps that have fallen
// outside the sliding window.
//
//   - If all timestamps for a key have expired, the key is deleted entirely
//     to prevent unbounded map growth.
//   - If only some timestamps have expired, the slice is trimmed to the valid
//     portion (b.history[k] = times[:valid]) so that the backing array can
//     eventually be reclaimed by the GC once no further appends re-use it.
//
// Call Sweep from a dedicated goroutine on a periodic ticker; the interval
// should be proportional to the window size (e.g. window / 2).
func (b *Breaker) Sweep() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.timeNow()
	cutoff := now.Add(-b.window)

	for k, times := range b.history {
		valid := 0
		for _, t := range times {
			if t.After(cutoff) {
				times[valid] = t
				valid++
			}
		}
		if valid == 0 {
			delete(b.history, k)
		} else {
			// Assign even when valid == len(times): the write makes the
			// compiler-visible that we intend the slice to be trimmed, and
			// ensures we never silently retain stale Time values past the end.
			b.history[k] = times[:valid]
		}
	}
}
