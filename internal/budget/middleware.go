// Package budget — see tracker.go for the package doc.
package budget

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	v1 "agentmesh/api/v1"
	"agentmesh/internal/ctxkeys"
	ihttp "agentmesh/internal/httputil"
)

// usageResponse is a minimal decode of the upstream LLM response body.
// Only the token usage fields are extracted; all other fields are ignored.
type usageResponse struct {
	Usage struct {
		TotalTokens int64 `json:"total_tokens"`
	} `json:"usage"`
}

// Middleware returns an http.Handler middleware that enforces per-tenant and
// per-agent token budgets using Redis.
//
// # Pre-flight check
//
// Before forwarding the request, the middleware checks both the tenant and
// agent counters. If either exceeds its limit the request is rejected with
// 402 Payment Required. Redis errors are handled according to the Tracker's
// configured failureMode:
//   - FailOpen:  allow the request through (availability over correctness).
//   - FailClosed: block the request with 503 Service Unavailable.
//
// # Eventual consistency note
//
// Budget enforcement is eventually consistent by one request. The upstream
// response is needed to know the actual token cost, so recording happens
// post-flight. A request that pushes a tenant exactly over budget will
// therefore be allowed through and recorded afterwards. This is a deliberate
// v1 tradeoff; v2 will use pre-authorised token reservations.
//
// # Post-flight recording
//
// After the upstream responds, the middleware reads total_tokens from the
// response body and calls RecordUsage. If syncRecord is false (the default),
// recording runs in a goroutine using a context detached from the request
// lifecycle (context.WithoutCancel) so cancellation of the inbound request
// does not abort the write.
func Middleware(tracker *Tracker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			// ------------------------------------------------------------------
			// Identify the tenant from context (injected by AuthMiddleware).
			// ------------------------------------------------------------------
			tenantID := ""
			agentID := r.Header.Get("X-Agent-ID")
			if agentID == "" {
				agentID = r.RemoteAddr
			}
			if tenant, ok := ctxkeys.GetTenant(ctx); ok {
				tenantID = tenant.TenantID
			}

			// Attach identity attributes to the current OTel span so that
			// budget decisions are visible in distributed traces.
			span := trace.SpanFromContext(ctx)
			span.SetAttributes(
				attribute.String("tenant_id", tenantID),
				attribute.String("agent_id", agentID),
			)

			// ------------------------------------------------------------------
			// Pre-flight: check tenant budget.
			// ------------------------------------------------------------------
			tenantExceeded, err := tracker.IsBudgetExceeded(ctx, TenantKey(tenantID), tracker.tenantLimit)
			if err != nil {
				if tracker.failureMode == v1.FailClosed {
					ihttp.WriteJSONError(w, http.StatusServiceUnavailable,
						"REDIS_UNAVAILABLE", "budget service temporarily unavailable")
					return
				}
				slog.Warn("budget: Redis unavailable (fail-open)",
					"tenant_id", tenantID,
					"agent_id", agentID,
					"scope", "tenant",
					"error", err,
				)
			} else if tenantExceeded {
				span.AddEvent("budget_exceeded", trace.WithAttributes(
					attribute.String("scope", "tenant"),
					attribute.String("tenant_id", tenantID),
				))
				slog.Warn("budget: tenant daily limit reached",
					"tenant_id", tenantID,
					"agent_id", agentID,
				)
				ihttp.WriteJSONError(w, http.StatusPaymentRequired,
					"BUDGET_EXCEEDED", "tenant daily token budget exceeded")
				return
			}

			// Pre-flight: check agent budget.
			agentExceeded, err := tracker.IsBudgetExceeded(ctx, AgentKey(agentID), tracker.agentLimit)
			if err != nil {
				if tracker.failureMode == v1.FailClosed {
					ihttp.WriteJSONError(w, http.StatusServiceUnavailable,
						"REDIS_UNAVAILABLE", "budget service temporarily unavailable")
					return
				}
				slog.Warn("budget: Redis unavailable (fail-open)",
					"tenant_id", tenantID,
					"agent_id", agentID,
					"scope", "agent",
					"error", err,
				)
			} else if agentExceeded {
				span.AddEvent("budget_exceeded", trace.WithAttributes(
					attribute.String("scope", "agent"),
					attribute.String("agent_id", agentID),
				))
				slog.Warn("budget: agent daily limit reached",
					"tenant_id", tenantID,
					"agent_id", agentID,
				)
				ihttp.WriteJSONError(w, http.StatusPaymentRequired,
					"BUDGET_EXCEEDED", "agent daily token budget exceeded")
				return
			}

			// ------------------------------------------------------------------
			// Serve the request, capturing the response body via ResponseRecorder.
			// ------------------------------------------------------------------
			rec := ihttp.NewResponseRecorder(w)
			defer rec.Free()

			next.ServeHTTP(rec, r)

			// ------------------------------------------------------------------
			// Post-flight: extract token usage from the upstream response and
			// record it asynchronously (or synchronously in tests).
			// ------------------------------------------------------------------
			var resp usageResponse
			if err := json.Unmarshal(rec.Body(), &resp); err != nil || resp.Usage.TotalTokens <= 0 {
				// Non-JSON response or zero tokens — nothing to record.
				return
			}

			tokens := resp.Usage.TotalTokens
			if tracker.syncRecord {
				if err := tracker.RecordUsage(ctx, tenantID, agentID, tokens); err != nil {
					slog.Error("budget: usage recording failed",
						"tenant_id", tenantID,
						"agent_id", agentID,
						"tokens", tokens,
						"error", err,
					)
				}
				return
			}

			// Detach from the request context so that client disconnection or
			// request cancellation cannot abort the Redis write.
			recordCtx := context.WithoutCancel(ctx)
			go func() {
				if err := tracker.RecordUsage(recordCtx, tenantID, agentID, tokens); err != nil {
					slog.Error("budget: usage recording failed (async)",
						"tenant_id", tenantID,
						"agent_id", agentID,
						"tokens", tokens,
						"error", err,
					)
				}
			}()
		})
	}
}
