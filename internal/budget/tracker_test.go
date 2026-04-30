package budget

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "agentmesh/api/v1"
)

// newTestTracker spins up a miniredis server and returns a Tracker wired to it
// together with a stop function. The Tracker always uses WithSyncRecording so
// that RecordUsage completes before assertions run.
func newTestTracker(t *testing.T, cfg *v1.BudgetConfig, failureMode v1.RedisFailureMode) (*Tracker, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	tracker := NewTracker(rdb, cfg, failureMode, WithSyncRecording())
	return tracker, mr
}

func defaultBudgetConfig() *v1.BudgetConfig {
	return &v1.BudgetConfig{
		PerTenantDailyUSD: 1.0,
		PerAgentDailyUSD:  0.5,
		TokensPerUSD:      100, // 1 USD = 100 tokens → limits: tenant=100, agent=50
	}
}

// ------------------------------------------------------------------ //
// RecordUsage — basic accumulation
// ------------------------------------------------------------------ //

func TestRecordUsage_SingleCall(t *testing.T) {
	t.Parallel()
	tracker, _ := newTestTracker(t, defaultBudgetConfig(), v1.FailOpen)
	ctx := context.Background()

	if err := tracker.RecordUsage(ctx, "tenant1", "agent1", 10); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	exceeded, err := tracker.IsBudgetExceeded(ctx, TenantKey("tenant1"), 100)
	if err != nil {
		t.Fatalf("IsBudgetExceeded: %v", err)
	}
	if exceeded {
		t.Error("10 tokens should not exceed limit of 100")
	}

	// Exact counter value via raw GET.
	got, err := tracker.rdb.Get(ctx, TenantKey("tenant1")).Int64()
	if err != nil {
		t.Fatalf("GET tenant key: %v", err)
	}
	if got != 10 {
		t.Errorf("tenant counter = %d, want 10", got)
	}
}

func TestRecordUsage_AccumulatesAcrossCalls(t *testing.T) {
	t.Parallel()
	tracker, _ := newTestTracker(t, defaultBudgetConfig(), v1.FailOpen)
	ctx := context.Background()

	for range 4 {
		if err := tracker.RecordUsage(ctx, "tenant1", "agent1", 10); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}

	got, err := tracker.rdb.Get(ctx, TenantKey("tenant1")).Int64()
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if got != 40 {
		t.Errorf("tenant counter after 4×10 = %d, want 40", got)
	}

	agentGot, err := tracker.rdb.Get(ctx, AgentKey("agent1")).Int64()
	if err != nil {
		t.Fatalf("GET agent: %v", err)
	}
	if agentGot != 40 {
		t.Errorf("agent counter after 4×10 = %d, want 40", agentGot)
	}
}

func TestRecordUsage_ZeroOrNegativeTokensNoOp(t *testing.T) {
	t.Parallel()
	tracker, _ := newTestTracker(t, defaultBudgetConfig(), v1.FailOpen)
	ctx := context.Background()

	if err := tracker.RecordUsage(ctx, "tenant1", "agent1", 0); err != nil {
		t.Fatalf("RecordUsage(0): %v", err)
	}
	if err := tracker.RecordUsage(ctx, "tenant1", "agent1", -5); err != nil {
		t.Fatalf("RecordUsage(-5): %v", err)
	}

	// Neither key should exist.
	if _, err := tracker.rdb.Get(ctx, TenantKey("tenant1")).Int64(); err != redis.Nil {
		t.Errorf("expected key to be absent, got err=%v", err)
	}
}

// ------------------------------------------------------------------ //
// ExpireNX — TTL is set on first call and NOT overwritten on subsequent calls
// ------------------------------------------------------------------ //

func TestRecordUsage_TTLSetOnFirstCall(t *testing.T) {
	t.Parallel()
	tracker, mr := newTestTracker(t, defaultBudgetConfig(), v1.FailOpen)
	ctx := context.Background()

	if err := tracker.RecordUsage(ctx, "tenant1", "agent1", 10); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	ttl := mr.TTL(TenantKey("tenant1"))
	if ttl <= 0 {
		t.Errorf("TTL after first RecordUsage = %v, want > 0", ttl)
	}
	// TTL should be close to 48 hours.
	if ttl > 49*time.Hour || ttl < 47*time.Hour {
		t.Errorf("TTL = %v, want ~48h", ttl)
	}
}

func TestRecordUsage_TTLNotExtendedOnSubsequentCalls(t *testing.T) {
	t.Parallel()
	tracker, mr := newTestTracker(t, defaultBudgetConfig(), v1.FailOpen)
	ctx := context.Background()

	// First call — sets the TTL.
	if err := tracker.RecordUsage(ctx, "tenant1", "agent1", 10); err != nil {
		t.Fatalf("first RecordUsage: %v", err)
	}
	ttlAfterFirst := mr.TTL(TenantKey("tenant1"))

	// Advance miniredis clock by 1 hour so the TTL decreases.
	mr.FastForward(time.Hour)

	// Second call — must NOT reset the TTL via ExpireNX semantics.
	if err := tracker.RecordUsage(ctx, "tenant1", "agent1", 10); err != nil {
		t.Fatalf("second RecordUsage: %v", err)
	}
	ttlAfterSecond := mr.TTL(TenantKey("tenant1"))

	// The TTL should have decreased by ~1 hour, not been reset to 48h.
	if ttlAfterSecond >= ttlAfterFirst {
		t.Errorf("TTL was not decremented: first=%v second=%v (ExpireNX should have been a no-op)",
			ttlAfterFirst, ttlAfterSecond)
	}

	// Confirm the counter did accumulate.
	got, _ := tracker.rdb.Get(ctx, TenantKey("tenant1")).Int64()
	if got != 20 {
		t.Errorf("tenant counter = %d, want 20", got)
	}
}

func TestRecordUsage_BothKeysTTLIndependent(t *testing.T) {
	t.Parallel()
	// If the tenant key already exists (from a previous test run / shared deployment),
	// the agent key's TTL must still be set independently.
	tracker, mr := newTestTracker(t, defaultBudgetConfig(), v1.FailOpen)
	ctx := context.Background()

	// Manually pre-create the tenant key with a short TTL to simulate an
	// already-existing key.
	if err := tracker.rdb.Set(ctx, TenantKey("tenant1"), 5, 10*time.Minute).Err(); err != nil {
		t.Fatalf("pre-create tenant key: %v", err)
	}

	// RecordUsage must set the agent key TTL even though the tenant key already
	// has a TTL (ExpireNX is a no-op for tenant but fires for agent).
	if err := tracker.RecordUsage(ctx, "tenant1", "agent1", 10); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	agentTTL := mr.TTL(AgentKey("agent1"))
	if agentTTL <= 0 {
		t.Errorf("agent TTL = %v after RecordUsage, want > 0", agentTTL)
	}
}

// ------------------------------------------------------------------ //
// IsBudgetExceeded — boundary conditions
// ------------------------------------------------------------------ //

func TestIsBudgetExceeded_MissingKeyNotExceeded(t *testing.T) {
	t.Parallel()
	tracker, _ := newTestTracker(t, defaultBudgetConfig(), v1.FailOpen)
	exceeded, err := tracker.IsBudgetExceeded(context.Background(), TenantKey("nonexistent"), 100)
	if err != nil {
		t.Fatalf("IsBudgetExceeded on missing key: %v", err)
	}
	if exceeded {
		t.Error("missing key should not be considered exceeded")
	}
}

func TestIsBudgetExceeded_ExactlyAtLimitIsExceeded(t *testing.T) {
	t.Parallel()
	tracker, _ := newTestTracker(t, defaultBudgetConfig(), v1.FailOpen)
	ctx := context.Background()

	// Record exactly 100 tokens (= tenant limit).
	if err := tracker.RecordUsage(ctx, "tenant1", "agent1", 100); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	exceeded, err := tracker.IsBudgetExceeded(ctx, TenantKey("tenant1"), 100)
	if err != nil {
		t.Fatalf("IsBudgetExceeded: %v", err)
	}
	if !exceeded {
		t.Error("counter == limit should be considered exceeded")
	}
}

func TestIsBudgetExceeded_OneBelowLimitNotExceeded(t *testing.T) {
	t.Parallel()
	tracker, _ := newTestTracker(t, defaultBudgetConfig(), v1.FailOpen)
	ctx := context.Background()

	if err := tracker.RecordUsage(ctx, "tenant1", "agent1", 99); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	exceeded, err := tracker.IsBudgetExceeded(ctx, TenantKey("tenant1"), 100)
	if err != nil {
		t.Fatalf("IsBudgetExceeded: %v", err)
	}
	if exceeded {
		t.Error("99 tokens should not exceed limit of 100")
	}
}

// ------------------------------------------------------------------ //
// Failure modes — Redis down
// ------------------------------------------------------------------ //

func TestRecordUsage_ReturnsErrorWhenRedisDown(t *testing.T) {
	t.Parallel()
	tracker, mr := newTestTracker(t, defaultBudgetConfig(), v1.FailOpen)

	// Bring down the miniredis server to simulate Redis unavailability.
	mr.Close()

	err := tracker.RecordUsage(context.Background(), "tenant1", "agent1", 10)
	if err == nil {
		t.Error("RecordUsage should return an error when Redis is unavailable")
	}
}

func TestIsBudgetExceeded_ReturnsErrorWhenRedisDown(t *testing.T) {
	t.Parallel()
	tracker, mr := newTestTracker(t, defaultBudgetConfig(), v1.FailOpen)
	ctx := context.Background()

	// Seed a value first so there is something to check, then kill Redis.
	_ = tracker.RecordUsage(ctx, "tenant1", "agent1", 10)
	mr.Close()

	_, err := tracker.IsBudgetExceeded(ctx, TenantKey("tenant1"), 100)
	if err == nil {
		t.Error("IsBudgetExceeded should return an error when Redis is unavailable")
	}
}

// ------------------------------------------------------------------ //
// Key helpers
// ------------------------------------------------------------------ //

func TestTenantKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tenantID string
		want     string
	}{
		{"acme", "budget:tenant:acme"},
		{"", "budget:tenant:"},
		{"my-tenant-123", "budget:tenant:my-tenant-123"},
	}
	for _, tc := range cases {
		if got := TenantKey(tc.tenantID); got != tc.want {
			t.Errorf("TenantKey(%q) = %q, want %q", tc.tenantID, got, tc.want)
		}
	}
}

func TestAgentKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		agentID string
		want    string
	}{
		{"agent-001", "budget:agent:agent-001"},
		{"", "budget:agent:"},
	}
	for _, tc := range cases {
		if got := AgentKey(tc.agentID); got != tc.want {
			t.Errorf("AgentKey(%q) = %q, want %q", tc.agentID, got, tc.want)
		}
	}
}

// ------------------------------------------------------------------ //
// NewTracker — TokensPerUSD fallback
// ------------------------------------------------------------------ //

func TestNewTracker_DefaultTokensPerUSD(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	cfg := &v1.BudgetConfig{
		PerTenantDailyUSD: 1.0,
		PerAgentDailyUSD:  0.5,
		TokensPerUSD:      0, // zero → falls back to defaultTokensPerUSD (1000)
	}
	tracker := NewTracker(rdb, cfg, v1.FailOpen)

	// 1.0 USD × 1000 tokens/USD = 1000 tokens
	if tracker.tenantLimit != 1000 {
		t.Errorf("tenantLimit = %d, want 1000 (default 1000 tokens/USD)", tracker.tenantLimit)
	}
	// 0.5 USD × 1000 tokens/USD = 500 tokens
	if tracker.agentLimit != 500 {
		t.Errorf("agentLimit = %d, want 500", tracker.agentLimit)
	}
}

func TestNewTracker_CustomTokensPerUSD(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	cfg := &v1.BudgetConfig{
		PerTenantDailyUSD: 2.0,
		PerAgentDailyUSD:  1.0,
		TokensPerUSD:      500, // custom rate
	}
	tracker := NewTracker(rdb, cfg, v1.FailOpen)

	if tracker.tenantLimit != 1000 { // 2.0 × 500
		t.Errorf("tenantLimit = %d, want 1000", tracker.tenantLimit)
	}
	if tracker.agentLimit != 500 { // 1.0 × 500
		t.Errorf("agentLimit = %d, want 500", tracker.agentLimit)
	}
}
