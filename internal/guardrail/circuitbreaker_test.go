package guardrail

import (
	"sync"
	"testing"
	"time"
)

// newTestBreaker returns a Breaker with a controllable clock. The returned
// *time.Time pointer advances the clock: simply assign a new value to move
// "now" forward without waiting for real time to pass.
func newTestBreaker(window time.Duration, limit int) (*Breaker, *time.Time) {
	now := time.Now()
	b := &Breaker{
		history: make(map[string][]time.Time),
		window:  window,
		limit:   limit,
		timeNow: func() time.Time { return now },
	}
	return b, &now
}

// ------------------------------------------------------------------ //
// Check — basic allow / block behaviour
// ------------------------------------------------------------------ //

func TestBreaker_EmptyHashNeverBlocks(t *testing.T) {
	t.Parallel()
	b, _ := newTestBreaker(time.Minute, 3)
	if b.Check("tenant", "agent", "") {
		t.Error("Check with empty hash should never block")
	}
	// history must remain empty — no key recorded
	if len(b.history) != 0 {
		t.Errorf("history should be empty, got %d entries", len(b.history))
	}
}

func TestBreaker_AllowsUpToLimit(t *testing.T) {
	t.Parallel()
	b, _ := newTestBreaker(time.Minute, 3)
	for i := range 3 {
		if b.Check("t1", "a1", "hash1") {
			t.Errorf("call %d: expected allowed, got blocked", i+1)
		}
	}
}

func TestBreaker_BlocksAtLimit(t *testing.T) {
	t.Parallel()
	b, _ := newTestBreaker(time.Minute, 3)
	for range 3 {
		b.Check("t1", "a1", "hash1")
	}
	if !b.Check("t1", "a1", "hash1") {
		t.Error("4th call should be blocked (limit=3)")
	}
}

func TestBreaker_BlockedCallNotAppended(t *testing.T) {
	t.Parallel()
	// After the breaker trips, the history length must not grow further.
	b, _ := newTestBreaker(time.Minute, 2)
	b.Check("t", "a", "h")
	b.Check("t", "a", "h") // limit reached
	b.Check("t", "a", "h") // blocked — must NOT be appended
	b.Check("t", "a", "h") // blocked — must NOT be appended

	key := "t|a|h"
	b.mu.Lock()
	n := len(b.history[key])
	b.mu.Unlock()
	if n != 2 {
		t.Errorf("history length = %d after blocking, want 2", n)
	}
}

func TestBreaker_DifferentKeysDontInterfere(t *testing.T) {
	t.Parallel()
	b, _ := newTestBreaker(time.Minute, 2)
	// Exhaust the limit for agent-1.
	b.Check("t1", "a1", "hash1")
	b.Check("t1", "a1", "hash1")
	// A different agent with the same hash should not be blocked.
	if b.Check("t1", "a2", "hash1") {
		t.Error("different agent should not be blocked by another agent's state")
	}
	// A different tenant should also be unaffected.
	if b.Check("t2", "a1", "hash1") {
		t.Error("different tenant should not be blocked by another tenant's state")
	}
	// A different hash for the same agent should also be unaffected.
	if b.Check("t1", "a1", "hash2") {
		t.Error("different hash should not be blocked")
	}
}

// ------------------------------------------------------------------ //
// Check — sliding window expiry
// ------------------------------------------------------------------ //

func TestBreaker_ExpiredEntriesAreNotCounted(t *testing.T) {
	t.Parallel()
	b, now := newTestBreaker(time.Minute, 3)

	// Record 3 calls at t=0 — breaker is now at the limit.
	b.Check("t", "a", "h")
	b.Check("t", "a", "h")
	b.Check("t", "a", "h")

	// Advance time past the window.
	*now = now.Add(2 * time.Minute)

	// All previous entries are now outside the window; the next call should
	// be allowed and record a fresh timestamp.
	if b.Check("t", "a", "h") {
		t.Error("call after window expiry should be allowed")
	}
}

func TestBreaker_PartialExpiryRespected(t *testing.T) {
	t.Parallel()
	b, now := newTestBreaker(time.Minute, 3)

	// t=0: 2 calls recorded.
	b.Check("t", "a", "h")
	b.Check("t", "a", "h")

	// Advance 90 seconds — these 2 entries expire.
	*now = now.Add(90 * time.Second)

	// t=90s: 2 fresh calls — both should be allowed (expired ones not counted).
	if b.Check("t", "a", "h") {
		t.Error("1st call after partial expiry should be allowed")
	}
	if b.Check("t", "a", "h") {
		t.Error("2nd call after partial expiry should be allowed")
	}
	// 3rd call hits the limit again.
	if b.Check("t", "a", "h") {
		t.Error("3rd call should be allowed (limit=3)")
	}
	if !b.Check("t", "a", "h") {
		t.Error("4th call should be blocked")
	}
}

// ------------------------------------------------------------------ //
// Sweep — memory reclamation
// ------------------------------------------------------------------ //

func TestSweep_RemovesEntirelyExpiredKey(t *testing.T) {
	t.Parallel()
	b, now := newTestBreaker(time.Minute, 5)

	b.Check("t", "a", "h")
	b.Check("t", "a", "h")

	// Advance past the window so all entries expire.
	*now = now.Add(2 * time.Minute)

	b.Sweep()

	b.mu.Lock()
	_, exists := b.history["t|a|h"]
	b.mu.Unlock()

	if exists {
		t.Error("Sweep should have deleted the key whose entire history expired")
	}
}

func TestSweep_RetainsValidEntriesAfterPartialExpiry(t *testing.T) {
	t.Parallel()
	b, now := newTestBreaker(time.Minute, 5)

	t0 := *now

	// 2 calls at t=0 (will expire).
	b.Check("t", "a", "h")
	b.Check("t", "a", "h")

	// Advance 90s; add 1 fresh call (will NOT expire in a 1-minute window
	// relative to the sweep time).
	*now = t0.Add(90 * time.Second)
	b.Check("t", "a", "h")

	// Sweep at t=90s: the 2 entries at t=0 are outside the 1-minute window,
	// the entry at t=90s is inside.
	b.Sweep()

	b.mu.Lock()
	remaining := len(b.history["t|a|h"])
	b.mu.Unlock()

	if remaining != 1 {
		t.Errorf("Sweep should leave 1 valid entry, got %d", remaining)
	}
}

func TestSweep_TrimsBackingSliceLength(t *testing.T) {
	t.Parallel()
	// Verify that after Sweep the slice length equals the number of valid
	// entries — i.e. the backing array is truly trimmed, not just re-sliced
	// to a longer underlying array while leaving stale data accessible.
	b, now := newTestBreaker(time.Minute, 5)

	t0 := *now

	// 3 calls at t=0 (will expire).
	b.Check("t", "a", "h")
	b.Check("t", "a", "h")
	b.Check("t", "a", "h")

	// 1 fresh call at t=90s (stays valid).
	*now = t0.Add(90 * time.Second)
	b.Check("t", "a", "h")

	b.Sweep()

	b.mu.Lock()
	s := b.history["t|a|h"]
	b.mu.Unlock()

	// Length of the slice as stored must equal 1 (the trimmed length).
	if len(s) != 1 {
		t.Errorf("slice len after Sweep = %d, want 1", len(s))
	}
}

func TestSweep_MultipleKeys(t *testing.T) {
	t.Parallel()
	b, now := newTestBreaker(time.Minute, 5)

	t0 := *now

	// key1: all calls at t=0, will fully expire.
	b.Check("t", "a", "h1")
	b.Check("t", "a", "h1")

	// key2: calls at t=0 and t=90s — mixed expiry.
	b.Check("t", "a", "h2")
	*now = t0.Add(90 * time.Second)
	b.Check("t", "a", "h2")

	// key3: only fresh call at t=90s — stays fully valid.
	b.Check("t", "a", "h3")

	b.Sweep()

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, exists := b.history["t|a|h1"]; exists {
		t.Error("fully expired key1 should have been deleted")
	}
	if got := len(b.history["t|a|h2"]); got != 1 {
		t.Errorf("key2 should have 1 valid entry, got %d", got)
	}
	if got := len(b.history["t|a|h3"]); got != 1 {
		t.Errorf("key3 should have 1 valid entry, got %d", got)
	}
}

func TestSweep_EmptyHistoryNoOp(t *testing.T) {
	t.Parallel()
	b, _ := newTestBreaker(time.Minute, 3)
	// Must not panic on empty history.
	b.Sweep()
	if len(b.history) != 0 {
		t.Error("Sweep on empty breaker should leave history empty")
	}
}

func TestSweep_NoEntriesExpiredNoKeyDeleted(t *testing.T) {
	t.Parallel()
	b, _ := newTestBreaker(time.Minute, 5)
	b.Check("t", "a", "h")
	b.Check("t", "a", "h")

	// Do NOT advance time — all entries are fresh.
	b.Sweep()

	b.mu.Lock()
	n := len(b.history["t|a|h"])
	b.mu.Unlock()

	if n != 2 {
		t.Errorf("Sweep should not remove fresh entries, got %d entries", n)
	}
}

// ------------------------------------------------------------------ //
// Concurrency safety
// ------------------------------------------------------------------ //

func TestBreaker_ConcurrentCheckIsSafe(t *testing.T) {
	t.Parallel()
	// Run a large number of goroutines concurrently to surface data races
	// when the -race flag is active.
	b, _ := newTestBreaker(time.Minute, 100)

	const goroutines = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(n int) {
			defer wg.Done()
			// Half use key A, half use key B to stress map access.
			hash := "hashA"
			if n%2 == 0 {
				hash = "hashB"
			}
			b.Check("tenant", "agent", hash)
		}(i)
	}
	wg.Wait()
}

func TestBreaker_ConcurrentSweepIsSafe(t *testing.T) {
	t.Parallel()
	b, _ := newTestBreaker(time.Minute, 100)

	var wg sync.WaitGroup
	// Concurrent writes.
	for i := range 50 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			b.Check("t", "a", "hash")
			_ = n
		}(i)
	}
	// Concurrent sweeps.
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Sweep()
		}()
	}
	wg.Wait()
}
