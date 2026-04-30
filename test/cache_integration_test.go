package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "agentmesh/api/v1"
	"agentmesh/internal/budget"
	"agentmesh/internal/cache"
	"agentmesh/internal/config"
	"agentmesh/internal/guardrail"
	"agentmesh/internal/proxy"
)

// ---------------------------------------------------------------------------
// Mock implementations (local to this file)
// ---------------------------------------------------------------------------

// cacheHitEmbedder satisfies cache.Embedder and returns a fixed stub vector.
type cacheHitEmbedder struct{}

func (cacheHitEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{1.0, 2.0, 3.0}, nil
}

// alwaysHitStore satisfies cache.VectorStore and always reports a cache hit
// with the pre-configured response payload.
type alwaysHitStore struct {
	response string
}

func (s *alwaysHitStore) Search(_ context.Context, tenantID string, _ []float32, _ float32) (*cache.CacheEntry, bool, error) {
	return &cache.CacheEntry{
		TenantID:  tenantID,
		Prompt:    "test",
		Response:  s.response,
		CreatedAt: time.Now().UTC(),
	}, true, nil
}

func (s *alwaysHitStore) Store(_ context.Context, _ cache.CacheEntry, _ []float32) error {
	// Cache hits never trigger a Store; this is a no-op stub.
	return nil
}

// ---------------------------------------------------------------------------
// Test
// ---------------------------------------------------------------------------

// TestCacheHitBypassesBudget proves that semantic cache hits completely bypass
// the Budget middleware — the upstream LLM is never called and no tokens are
// deducted from Redis, even after many requests.
//
// Stack: AuthMiddleware → Guardrail → Cache(alwaysHit) → Budget → HandleProxy
//
// With an agent budget of 25 tokens and 10 tokens-per-request, the budget
// middleware would block request 4 if cache hits were not intercepting.
// All 5 requests must return 200 and Redis must show 0 tokens consumed.
func TestCacheHitBypassesBudget(t *testing.T) {
	t.Parallel()

	// --- Mock upstream (should never be reached) ----------------------------
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream LLM was called — cache hit should have short-circuited the chain")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	// --- miniredis for budget tracking --------------------------------------
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// --- Config -------------------------------------------------------------
	const (
		cacheTestTenantID    = "tenant-cache-test"
		cacheTestInboundKey  = "cache-inbound-key"
		cacheTestUpstreamKey = "cache-upstream-key"
		cacheTestAgentID     = "agent-cache-001"
		agentTokenLimit      = int64(25) // budget trips on request 4 without cache
	)

	cachedBody := `{"choices":[{"message":{"content":"cached response"}}],"usage":{"total_tokens":10}}`

	cfg := &v1.Config{
		Version: "v1",
		Server:  v1.ServerConfig{ProxyPort: 8080, AdminPort: 9090},
		Tenants: []v1.TenantConfig{
			{
				TenantID:       cacheTestTenantID,
				APIKey:         "[REDACTED]",
				UpstreamURL:    upstream.URL,
				UpstreamAPIKey: "[REDACTED]",
			},
		},
		Budget: v1.BudgetConfig{
			PerAgentDailyUSD:  float64(agentTokenLimit) / 1000.0,
			PerTenantDailyUSD: 999.0,
		},
		Redis:      v1.RedisConfig{Address: mr.Addr()},
		Guardrails: v1.GuardrailConfig{},
	}

	lc := &config.LoadedConfig{
		Config: cfg,
		TenantMap: map[string]*v1.TenantConfig{
			cacheTestInboundKey: &cfg.Tenants[0],
		},
		UpstreamKeyMap: map[string]string{
			cacheTestTenantID: cacheTestUpstreamKey,
		},
	}

	// --- Middleware stack ----------------------------------------------------
	breaker := guardrail.NewBreaker(5*time.Minute, 100) // high limit; loop detection not under test

	tracker := budget.NewTracker(rdb, &cfg.Budget, v1.FailClosed,
		budget.WithSyncRecording())

	cacheMiddleware := cache.Middleware(
		&alwaysHitStore{response: cachedBody},
		cacheHitEmbedder{},
		cache.Config{SimilarityThreshold: 0.90},
	)

	srv, err := proxy.NewServer(lc)
	if err != nil {
		t.Fatalf("proxy.NewServer: %v", err)
	}

	// Wire: AuthMiddleware → Guardrail → Cache → Budget → HandleProxy
	srv.RegisterChain(
		srv.GuardrailMiddleware(breaker),
		cacheMiddleware,
		budget.Middleware(tracker),
	)

	handler := srv.Mux()

	// --- Send 5 requests — all should return 200 from the cache -------------
	body := buildChatBody(t, "explain quantum entanglement")

	for i := 1; i <= 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cacheTestInboundKey)
		req.Header.Set("X-Agent-ID", cacheTestAgentID)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("request %d: want 200 got %d — body: %s", i, rec.Code, rec.Body.String())
		}

		if !strings.Contains(rec.Body.String(), "cached response") {
			t.Errorf("request %d: body %q does not contain \"cached response\"", i, rec.Body.String())
		}

		if got := rec.Header().Get("X-AgentMesh-Cache"); got != "HIT" {
			t.Errorf("request %d: X-AgentMesh-Cache = %q, want HIT", i, got)
		}
	}

	// --- Assert zero tokens consumed ----------------------------------------
	// Cache hits never reach the Budget middleware, so the agent key must
	// not exist in Redis at all (redis.Nil → 0 usage).
	agentKey := budget.AgentKey(cacheTestAgentID)
	val, err := rdb.Get(context.Background(), agentKey).Int64()
	if err != nil && err.Error() != "redis: nil" {
		t.Fatalf("unexpected Redis error: %v", err)
	}
	if val != 0 {
		t.Errorf("agent token usage = %d, want 0 — cache hits should not deduct from budget", val)
	}
}
