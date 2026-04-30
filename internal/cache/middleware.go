// Package cache — see ports.go for the package doc.
package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"agentmesh/internal/ctxkeys"
	ihttp "agentmesh/internal/httputil"
)

// Config holds the tunable parameters for the semantic caching middleware.
type Config struct {
	// SimilarityThreshold is the minimum cosine similarity score required for
	// a Qdrant result to be considered a cache hit. Values closer to 1.0 are
	// stricter (near-exact matches only); values around 0.85–0.90 catch
	// paraphrased prompts. Must be in the range (0, 1].
	SimilarityThreshold float32
}

// cacheRequest is the minimal OpenAI Chat Completions shape the middleware
// needs in order to extract the last user-role prompt for embedding.
// Other fields are intentionally ignored.
type cacheRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// lastUserContent returns the content of the last message whose role is "user",
// or an empty string when no such message exists.
func lastUserContent(req cacheRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if strings.EqualFold(req.Messages[i].Role, "user") {
			return req.Messages[i].Content
		}
	}
	return ""
}

// Middleware returns an HTTP middleware that intercepts requests and serves
// semantically equivalent cached responses when available. On a cache miss it
// forwards the request to next, then asynchronously stores a successful
// upstream response for future reuse.
//
// Placement in the chain: insert between GuardrailMiddleware and
// BudgetMiddleware so that cache hits bypass both token counting and upstream
// calls entirely.
//
//	srv.RegisterChain(
//	  srv.GuardrailMiddleware(breaker),
//	  cache.Middleware(store, embedder, cfg),  // ← here
//	  budget.Middleware(tracker),
//	)
func Middleware(store VectorStore, embedder Embedder, cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// ----------------------------------------------------------------
			// 1. Resolve tenant — required for scoped cache isolation.
			// ----------------------------------------------------------------
			tenant, ok := ctxkeys.GetTenant(r.Context())
			if !ok {
				ihttp.WriteJSONError(w, http.StatusInternalServerError,
					"MISSING_TENANT", "tenant not found in request context")
				return
			}
			tenantID := tenant.TenantID

			// Attach tenant identity to the OTel span for distributed tracing.
			// Agent ID is not tracked at the cache layer; caching is tenant-scoped.
			span := trace.SpanFromContext(r.Context())
			span.SetAttributes(attribute.String("tenant_id", tenantID))

			// ----------------------------------------------------------------
			// 2. Extract prompt from the request body.
			//    Peek at the body without consuming it — restore it before
			//    calling next so the reverse proxy can forward it intact.
			// ----------------------------------------------------------------
			if r.Body == nil {
				next.ServeHTTP(w, r)
				return
			}

			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				// Non-JSON request: caching does not apply.
				next.ServeHTTP(w, r)
				return
			}

			// NOTE: The body has already been size-limited by GuardrailMiddleware
			// (1 MiB cap). This ReadAll is safe because r.Body was replaced with a
			// bounded bytes.Reader before this middleware ran. If the chain order
			// is ever changed so CacheMiddleware runs before GuardrailMiddleware,
			// wrap this in an io.LimitReader to preserve the safety invariant.
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				// Unreadable body — fall through; guardrail/proxy will handle.
				next.ServeHTTP(w, r)
				return
			}
			// Restore body for downstream handlers (reverse proxy needs it).
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

			var req cacheRequest
			if err := json.Unmarshal(bodyBytes, &req); err != nil {
				// Malformed JSON — fall through; let the upstream return 400.
				next.ServeHTTP(w, r)
				return
			}

			prompt := lastUserContent(req)
			if prompt == "" {
				// No user prompt to embed — caching does not apply.
				next.ServeHTTP(w, r)
				return
			}

			// ----------------------------------------------------------------
			// 3. Generate the prompt embedding.
			// ----------------------------------------------------------------
			embedding, err := embedder.Embed(r.Context(), prompt)
			if err != nil {
				// Fail open: log and fall through rather than blocking traffic.
				slog.Warn("cache: embedding failed, skipping cache lookup",
					"tenant_id", tenantID,
					"error", err,
				)
				next.ServeHTTP(w, r)
				return
			}

			// ----------------------------------------------------------------
			// 4. Query the vector store.
			// ----------------------------------------------------------------
			entry, hit, err := store.Search(r.Context(), tenantID, embedding, cfg.SimilarityThreshold)
			if err != nil {
				// Fail open: vector store errors are non-fatal for the caller.
				slog.Warn("cache: search failed, skipping cache lookup",
					"tenant_id", tenantID,
					"error", err,
				)
				next.ServeHTTP(w, r)
				return
			}

			// ----------------------------------------------------------------
			// 5. Cache HIT: replay the stored response directly to the client.
			//    The request does not reach the budget tracker or upstream LLM.
			// ----------------------------------------------------------------
			if hit {
				span.AddEvent("cache_hit", trace.WithAttributes(
					attribute.String("tenant_id", tenantID),
				))
				// Estimate token savings from the cached response's usage field.
				// The prompt itself is never logged to preserve the zero-trust
				// security posture.
				var savedUsage struct {
					Usage struct {
						TotalTokens int64 `json:"total_tokens"`
					} `json:"usage"`
				}
				_ = json.Unmarshal([]byte(entry.Response), &savedUsage)
				slog.Debug("cache: HIT — upstream LLM call skipped",
					"tenant_id", tenantID,
					"tokens_saved", savedUsage.Usage.TotalTokens,
				)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-AgentMesh-Cache", "HIT")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, entry.Response)
				return
			}

			// ----------------------------------------------------------------
			// 6. Cache MISS: record the upstream response so we can store it.
			// ----------------------------------------------------------------
			w.Header().Set("X-AgentMesh-Cache", "MISS")
			rec := ihttp.NewResponseRecorder(w)
			defer rec.Free()

			next.ServeHTTP(rec, r)

			// ----------------------------------------------------------------
			// 7. Post-flight async store on successful upstream responses.
			//    context.WithoutCancel ensures the goroutine is not aborted
			//    when the HTTP request context is cancelled after the response
			//    is flushed to the client.
			// ----------------------------------------------------------------
			if status := rec.Status(); status >= http.StatusOK && status < http.StatusMultipleChoices {
				// Capture the body before rec is freed by the deferred Free().
				// We take a copy so the pool buffer can be safely recycled.
				bodySnapshot := make([]byte, len(rec.Body()))
				copy(bodySnapshot, rec.Body())

				storeCtx := context.WithoutCancel(r.Context())
				go func() {
					newEntry := Entry{
						TenantID:  tenantID,
						Prompt:    prompt,
						Response:  string(bodySnapshot),
						CreatedAt: time.Now().UTC(),
					}
					if err := store.Store(storeCtx, newEntry, embedding); err != nil {
						slog.Error("cache: failed to store entry",
							"tenant_id", tenantID,
							"error", err,
						)
					}
				}()
			}
		})
	}
}
