package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "agentmesh/api/v1"
	"agentmesh/internal/budget"
	"agentmesh/internal/config"
	"agentmesh/internal/guardrail"
	"agentmesh/internal/proxy"
)

// tokensPerRequest is the fixed number of tokens the mock LLM returns.
const tokensPerRequest = 10

// testInboundKey is the bearer token callers send to agentmesh.
const testInboundKey = "integration-inbound-key"

// testUpstreamKey is the credential agentmesh must forward to the LLM upstream.
const testUpstreamKey = "integration-upstream-key"

// testTenantID identifies the single tenant used in all integration tests.
const testTenantID = "tenant-integration"

// testAgentID is the value sent in X-Agent-ID for all requests.
const testAgentID = "agent-001"

// mockLLMResponse returns the JSON body the mock upstream sends back.
// Using json.Marshal prevents any chance of injection from the prompt value.
func mockLLMResponse(t *testing.T) string {
	t.Helper()
	type usage struct {
		TotalTokens int `json:"total_tokens"`
	}
	type resp struct {
		Usage usage `json:"usage"`
	}
	b, err := json.Marshal(resp{Usage: usage{TotalTokens: tokensPerRequest}})
	if err != nil {
		t.Fatalf("marshal mock response: %v", err)
	}
	return string(b)
}

// buildChatBody builds a safe, injection-proof OpenAI-style chat request body.
// fmt.Sprintf(%q, prompt) is used so special characters are JSON-escaped.
func buildChatBody(t *testing.T, prompt string) string {
	t.Helper()
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type req struct {
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
		Stream   bool      `json:"stream"`
	}
	b, err := json.Marshal(req{
		Model:    "gpt-4o",
		Messages: []message{{Role: "user", Content: prompt}},
		Stream:   false,
	})
	if err != nil {
		t.Fatalf("marshal chat body: %v", err)
	}
	return string(b)
}

// newTestStack wires a full agentmesh middleware chain backed by a miniredis
// instance and the provided mock upstream. It returns the mux (to call
// ServeHTTP against) and a cleanup function.
//
// Parameters:
//   - breakerLimit:   max identical prompts per window before 429
//   - agentLimitTokens: max tokens per agent before 402
func newTestStack(
	t *testing.T,
	upstreamURL string,
	breakerLimit int,
	agentLimitTokens int64,
) http.Handler {
	t.Helper()

	// --- miniredis ----------------------------------------------------------
	mr := miniredis.RunT(t)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// --- Config + LoadedConfig ----------------------------------------------
	// PerAgentDailyUSD is set to the desired token limit / 1000 (defaultTokensPerUSD).
	// PerTenantDailyUSD is set very high so only the per-agent limit is
	// exercised in the budget exhaustion test.
	cfg := &v1.Config{
		Version: "v1",
		Server:  v1.ServerConfig{ProxyPort: 8080, AdminPort: 9090},
		Tenants: []v1.TenantConfig{
			{
				TenantID:       testTenantID,
				APIKey:         "[REDACTED]",
				UpstreamURL:    upstreamURL,
				UpstreamAPIKey: "[REDACTED]",
			},
		},
		Budget: v1.BudgetConfig{
			PerAgentDailyUSD:  float64(agentLimitTokens) / 1000.0,
			PerTenantDailyUSD: 999.0, // effectively unlimited for these tests
		},
		Redis:      v1.RedisConfig{Address: mr.Addr()},
		Guardrails: v1.GuardrailConfig{},
	}

	lc := &config.LoadedConfig{
		Config: cfg,
		TenantMap: map[string]*v1.TenantConfig{
			testInboundKey: &cfg.Tenants[0],
		},
		UpstreamKeyMap: map[string]string{
			testTenantID: testUpstreamKey,
		},
	}

	// --- Middleware stack ---------------------------------------------------
	breaker := guardrail.NewBreaker(5*time.Minute, breakerLimit)

	tracker := budget.NewTracker(rdb, &cfg.Budget, v1.FailClosed,
		budget.WithSyncRecording())

	srv, err := proxy.NewServer(lc)
	if err != nil {
		t.Fatalf("proxy.NewServer: %v", err)
	}

	// Wire: AuthMiddleware → GuardrailMiddleware → BudgetMiddleware → HandleProxy
	srv.RegisterChain(
		srv.GuardrailMiddleware(breaker),
		budget.Middleware(tracker),
	)

	return srv.Mux()
}

// newRequest builds an HTTP request with the correct headers for an
// agentmesh proxy call. JSON body must be pre-built using buildChatBody.
func newRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testInboundKey)
	req.Header.Set("X-Agent-ID", testAgentID)
	return req
}

// --- Loop detection test ---------------------------------------------------

// TestLoopDetection verifies that the guardrail breaker allows the first
// `breakerLimit` identical prompts and blocks the (breakerLimit+1)-th with 429.
func TestLoopDetection(t *testing.T) {
	t.Parallel()

	const breakerLimit = 3 // trips on 4th identical prompt

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockLLMResponse(t)))
	}))
	t.Cleanup(upstream.Close)

	handler := newTestStack(t, upstream.URL, breakerLimit, 999_000)

	prompt := "explain quantum entanglement to me"
	body := buildChatBody(t, prompt)

	for i := 1; i <= breakerLimit+1; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, newRequest(t, body))

		if i <= breakerLimit {
			if rec.Code != http.StatusOK {
				t.Errorf("request %d: want 200, got %d", i, rec.Code)
			}
		} else {
			// The (breakerLimit+1)-th identical prompt must be blocked.
			if rec.Code != http.StatusTooManyRequests {
				t.Errorf("request %d (should be blocked): want 429, got %d — body: %s",
					i, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "LOOP_DETECTED") {
				t.Errorf("request %d: body %q does not contain LOOP_DETECTED", i, rec.Body.String())
			}
		}
	}
}

// --- Budget exhaustion test ------------------------------------------------

// TestAgentBudgetExhaustion validates the eventual-consistency budget model.
//
// Budget:   agentLimit = 25 tokens
// Cost:     10 tokens per request (returned by the mock upstream)
//
// Because enforcement is post-flight by one request, the sequence is:
//
//	Request 1 → pre-flight: 0  < 25 → PASS; post-flight: records 10
//	Request 2 → pre-flight: 10 < 25 → PASS; post-flight: records 20
//	Request 3 → pre-flight: 20 < 25 → PASS; post-flight: records 30
//	Request 4 → pre-flight: 30 ≥ 25 → BLOCKED (402)
func TestAgentBudgetExhaustion(t *testing.T) {
	t.Parallel()

	const agentLimitTokens = 25

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockLLMResponse(t)))
	}))
	t.Cleanup(upstream.Close)

	// Use a very high breaker limit so loop detection never fires here.
	handler := newTestStack(t, upstream.URL, 1000, agentLimitTokens)

	type result struct {
		req    int
		status int
	}
	var results []result

	for i := 1; i <= 4; i++ {
		// Use distinct prompts so the guardrail breaker is never triggered.
		prompt := "unique budget test prompt number " + strings.Repeat("x", i)
		body := buildChatBody(t, prompt)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, newRequest(t, body))
		results = append(results, result{i, rec.Code})
	}

	// Requests 1, 2, 3 must pass; request 4 must be blocked with 402.
	for _, r := range results[:3] {
		if r.status != http.StatusOK {
			t.Errorf("request %d: want 200 (budget not yet exceeded), got %d", r.req, r.status)
		}
	}
	last := results[3]
	if last.status != http.StatusPaymentRequired {
		t.Errorf("request %d: want 402 (budget exhausted), got %d", last.req, last.status)
	}
}
