// Package budget implements Redis-backed per-tenant and per-agent token budget
// enforcement for the AgentMesh request pipeline.
//
// Each incoming request passes through two checks:
//
//  1. Pre-flight: the current token counter for both the tenant and the agent
//     is read from Redis. If either exceeds its configured limit the request is
//     rejected with 402 Payment Required before any upstream call is made.
//
//  2. Post-flight: after a successful upstream response the actual token cost
//     (read from the usage.total_tokens field in the LLM response body) is
//     recorded in Redis via an atomic MULTI/EXEC pipeline (INCRBY + ExpireNX).
//     Recording is performed in a goroutine detached from the request context
//     so that client disconnection cannot abort the Redis write.
//
// Budget enforcement is eventually consistent by one request: because the
// token cost is unknown until the upstream responds, a single request may
// overshoot the limit by its own cost. This is a deliberate v1 tradeoff;
// v2 will introduce pre-authorised token reservations.
//
// Redis key layout:
//
//	budget:tenant:<TenantID>  →  cumulative tokens consumed today
//	budget:agent:<AgentID>    →  cumulative tokens consumed today
//
// Both keys carry a 48-hour TTL set via ExpireNX (set-if-not-exists) so
// that the window resets naturally without a separate cron job, and
// concurrent requests cannot inadvertently extend the window.
package budget

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	v1 "agentmesh/api/v1"
)

const (
	// budgetTTL is the TTL applied (via ExpireNX) to every budget key.
	// 48 hours gives a rolling two-day window that naturally resets without
	// a separate cron job.
	budgetTTL = 48 * time.Hour

	// defaultTokensPerUSD is the fallback conversion rate when TokensPerUSD is
	// not set in config. 1 USD ≈ 1 000 tokens is a rough approximation for
	// GPT-4-class models; operators should override this in config to match
	// their actual model pricing.
	defaultTokensPerUSD = 1000.0

	// keyPrefixTenant is the Redis key namespace for per-tenant token usage.
	keyPrefixTenant = "budget:tenant:"

	// keyPrefixAgent is the Redis key namespace for per-agent token usage.
	keyPrefixAgent = "budget:agent:"
)

// Tracker records and checks token-budget usage against Redis.
type Tracker struct {
	rdb         *redis.Client
	failureMode v1.RedisFailureMode

	// syncRecord controls whether RecordUsage is called synchronously.
	// When false (the default), the budget middleware fires recording in a
	// goroutine so the HTTP response is not blocked. Set to true in tests via
	// WithSyncRecording() to avoid races on test teardown.
	syncRecord bool

	// per-tenant and per-agent daily limits (in tokens)
	tenantLimit int64
	agentLimit  int64
}

// NewTracker constructs a Tracker backed by the provided Redis client.
// opts are functional options applied before the Tracker is returned.
func NewTracker(rdb *redis.Client, cfg *v1.BudgetConfig, failureMode v1.RedisFailureMode, opts ...func(*Tracker)) *Tracker {
	tokensPerUSD := cfg.TokensPerUSD
	if tokensPerUSD <= 0 {
		tokensPerUSD = defaultTokensPerUSD
	}
	t := &Tracker{
		rdb:         rdb,
		failureMode: failureMode,
		tenantLimit: int64(cfg.PerTenantDailyUSD * tokensPerUSD),
		agentLimit:  int64(cfg.PerAgentDailyUSD * tokensPerUSD),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// WithSyncRecording returns a functional option that forces RecordUsage to run
// synchronously instead of in a goroutine. Use this in unit tests to ensure
// recording completes before assertions are made.
func WithSyncRecording() func(*Tracker) {
	return func(t *Tracker) {
		t.syncRecord = true
	}
}

// RecordUsage increments the token counters for both the tenant and the agent.
// For each key it:
//  1. Increments the counter by tokens using INCRBY.
//  2. Calls ExpireNX to set a 48-hour TTL only if one is not already set.
//     This prevents concurrent requests from inadvertently resetting the
//     expiry and extending the window indefinitely.
//
// TxPipeline (MULTI/EXEC) is used so that INCRBY and ExpireNX execute
// atomically. If the connection is lost mid-pipeline, Redis discards the
// entire transaction, preventing a key from ending up with a counter but
// no TTL (which would make it live forever and permanently block the budget).
func (t *Tracker) RecordUsage(ctx context.Context, tenantID, agentID string, tokens int64) error {
	if tokens <= 0 {
		return nil
	}

	tenantKey := keyPrefixTenant + tenantID
	agentKey := keyPrefixAgent + agentID

	pipe := t.rdb.TxPipeline()
	pipe.IncrBy(ctx, tenantKey, tokens)
	pipe.ExpireNX(ctx, tenantKey, budgetTTL)
	pipe.IncrBy(ctx, agentKey, tokens)
	pipe.ExpireNX(ctx, agentKey, budgetTTL)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("budget.RecordUsage: %w", err)
	}
	return nil
}

// IsBudgetExceeded reports whether the given key (a pre-built Redis key such as
// "budget:tenant:<id>") has surpassed limit tokens.
//
// A Redis error is returned as the second return value so callers can apply
// the configured failureMode policy.
func (t *Tracker) IsBudgetExceeded(ctx context.Context, key string, limit int64) (exceeded bool, err error) {
	val, err := t.rdb.Get(ctx, key).Int64()
	if err == redis.Nil {
		// Key not present → no usage recorded yet → budget not exceeded.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("budget.IsBudgetExceeded(%q): %w", key, err)
	}
	return val >= limit, nil
}

// TenantKey returns the Redis key for a tenant's budget counter.
func TenantKey(tenantID string) string { return keyPrefixTenant + tenantID }

// AgentKey returns the Redis key for an agent's budget counter.
func AgentKey(agentID string) string { return keyPrefixAgent + agentID }
