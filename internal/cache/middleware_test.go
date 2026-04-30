package cache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "agentmesh/api/v1"
	"agentmesh/internal/ctxkeys"
)

// ---------------------------------------------------------------------------
// Mock implementations
// ---------------------------------------------------------------------------

// mockEmbedder satisfies Embedder and returns a fixed vector.
type mockEmbedder struct{}

func (mockEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{1.0, 2.0}, nil
}

// mockVectorStore satisfies VectorStore with configurable hit/miss behaviour.
// StoreChan receives every entry passed to Store so tests can synchronise on
// the async goroutine without time.Sleep.
type mockVectorStore struct {
	Hit       bool
	StoreChan chan CacheEntry
}

func (m *mockVectorStore) Search(_ context.Context, _ string, _ []float32, _ float32) (*CacheEntry, bool, error) {
	if m.Hit {
		return &CacheEntry{
			TenantID:  "tenant-test",
			Prompt:    "hello",
			Response:  `{"cached": true}`,
			CreatedAt: time.Now().UTC(),
		}, true, nil
	}
	return nil, false, nil
}

func (m *mockVectorStore) Store(_ context.Context, entry CacheEntry, _ []float32) error {
	m.StoreChan <- entry
	return nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// testTenant builds a minimal TenantConfig for context injection.
var testTenant = &v1.TenantConfig{
	TenantID:    "tenant-test",
	APIKey:      "[REDACTED]",
	UpstreamURL: "https://api.example.com",
}

// chatBody is a minimal valid OpenAI-style request body.
const chatBody = `{"messages":[{"role":"user","content":"hello"}]}`

// newCacheRequest builds a POST request with the tenant injected into the
// context and a JSON body ready for the middleware to parse.
func newCacheRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(chatBody))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(ctxkeys.WithTenant(req.Context(), testTenant))
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestCacheMiddleware_Hit verifies that when the vector store reports a hit
// the middleware replays the cached response to the client immediately —
// without calling next (i.e. without touching the budget tracker or the LLM).
func TestCacheMiddleware_Hit(t *testing.T) {
	t.Parallel()

	store := &mockVectorStore{
		Hit:       true,
		StoreChan: make(chan CacheEntry, 1),
	}
	embedder := mockEmbedder{}
	cfg := Config{SimilarityThreshold: 0.9}

	// nextHandler must never be reached on a cache hit.
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called on a cache hit — request was not short-circuited")
	})

	mw := Middleware(store, embedder, cfg)(next)

	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, newCacheRequest(t))

	if got := rr.Code; got != http.StatusOK {
		t.Errorf("status = %d, want %d", got, http.StatusOK)
	}

	const wantBody = `{"cached": true}`
	if got := rr.Body.String(); got != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}

	if got := rr.Header().Get("X-AgentMesh-Cache"); got != "HIT" {
		t.Errorf("X-AgentMesh-Cache = %q, want %q", got, "HIT")
	}
}

// TestCacheMiddleware_Miss verifies that on a cache miss the middleware
// forwards the request to next, sets the MISS header, and asynchronously
// stores the upstream response in the vector store.
func TestCacheMiddleware_Miss(t *testing.T) {
	t.Parallel()

	const upstreamBody = `{"upstream": "success"}`

	store := &mockVectorStore{
		Hit: false,
		// Buffered so the goroutine never blocks if the test is slow to read.
		StoreChan: make(chan CacheEntry, 1),
	}
	embedder := mockEmbedder{}
	cfg := Config{SimilarityThreshold: 0.9}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upstreamBody))
	})

	mw := Middleware(store, embedder, cfg)(next)

	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, newCacheRequest(t))

	if got := rr.Code; got != http.StatusOK {
		t.Errorf("status = %d, want %d", got, http.StatusOK)
	}

	if got := rr.Body.String(); got != upstreamBody {
		t.Errorf("body = %q, want %q", got, upstreamBody)
	}

	if got := rr.Header().Get("X-AgentMesh-Cache"); got != "MISS" {
		t.Errorf("X-AgentMesh-Cache = %q, want %q", got, "MISS")
	}

	// Wait for the async Store goroutine to complete.
	// The select with a 1-second timeout prevents both flakes (goroutine not
	// yet scheduled) and infinite hangs (goroutine never runs).
	select {
	case stored := <-store.StoreChan:
		if stored.TenantID != testTenant.TenantID {
			t.Errorf("stored TenantID = %q, want %q", stored.TenantID, testTenant.TenantID)
		}
		if stored.Response != upstreamBody {
			t.Errorf("stored Response = %q, want %q", stored.Response, upstreamBody)
		}
		if stored.Prompt != "hello" {
			t.Errorf("stored Prompt = %q, want %q", stored.Prompt, "hello")
		}
		if stored.CreatedAt.IsZero() {
			t.Error("stored CreatedAt is zero")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async Store to be called")
	}
}
