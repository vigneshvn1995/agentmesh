// Package proxy — see server.go for the package doc.
package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"agentmesh/internal/ctxkeys"
	"agentmesh/internal/guardrail"
	ihttp "agentmesh/internal/httputil"
)

const (
	// maxBodyBytes is the maximum request body size the guardrail will read.
	// Requests larger than this are rejected before any parsing occurs.
	maxBodyBytes = 1 << 20 // 1 MiB
)

// openAIRequest is a targeted, minimal decode of the OpenAI Chat Completions
// request body. Only the fields needed by the guardrail are unmarshalled; all
// other fields are captured by RawMessage so they survive re-encoding intact.
type openAIRequest struct {
	// Stream is true when the caller has requested Server-Sent Events output.
	Stream bool `json:"stream"`

	// Messages holds the conversation turns. We only need the last user message
	// for normalisation; the array is decoded lazily below.
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// GuardrailMiddleware returns an http.Handler middleware that:
//  1. Enforces a 1 MiB request body limit.
//  2. Blocks streaming requests (stream: true) with 501 Not Implemented.
//  3. Extracts the latest user-role prompt, normalises and hashes it.
//  4. Calls the sliding-window Breaker; trips with 429 on LOOP_DETECTED.
//  5. Restores the body as an io.NopCloser so downstream handlers can read it.
func (s *Server) GuardrailMiddleware(breaker *guardrail.Breaker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// ----------------------------------------------------------------
			// Instrument: attach tenant/agent identity to the OTel span so
			// that guardrail decisions appear in distributed traces.
			// Prompt content is never set as a span attribute or log field
			// to preserve the zero-trust security posture.
			// ----------------------------------------------------------------
			span := trace.SpanFromContext(r.Context())
			agentID := r.Header.Get("X-Agent-ID")
			if agentID == "" {
				agentID = r.RemoteAddr
			}
			tenantID := ""
			if tenant, ok := ctxkeys.GetTenant(r.Context()); ok {
				tenantID = tenant.TenantID
			}
			span.SetAttributes(
				attribute.String("tenant_id", tenantID),
				attribute.String("agent_id", agentID),
			)

			// ----------------------------------------------------------------
			// 1. Body size limit
			// ----------------------------------------------------------------
			// Guard against nil body (valid for GET / HEAD requests).
			if r.Body == nil {
				r.Body = http.NoBody
			}
			limitedReader := io.LimitReader(r.Body, maxBodyBytes+1)
			bodyBytes, err := io.ReadAll(limitedReader)
			if err != nil {
				ihttp.WriteJSONError(w, http.StatusBadRequest,
					"BODY_READ_ERROR", "failed to read request body")
				return
			}

			if len(bodyBytes) > maxBodyBytes {
				ihttp.WriteJSONError(w, http.StatusRequestEntityTooLarge,
					"BODY_TOO_LARGE", "request body exceeds 1 MiB limit")
				return
			}

			// ----------------------------------------------------------------
			// 2. Restore the body so downstream handlers (the reverse proxy)
			//    can read it from the start.
			// ----------------------------------------------------------------
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

			// ----------------------------------------------------------------
			// 3. Parse only what the guardrail needs; ignore non-JSON bodies
			//    (e.g. health checks) without blocking them.
			// ----------------------------------------------------------------
			if !isJSONContentType(r) {
				next.ServeHTTP(w, r)
				return
			}

			var req openAIRequest
			if err := json.Unmarshal(bodyBytes, &req); err != nil {
				// Malformed JSON: let the upstream return its own error.
				// Log at debug so operators can diagnose missing loop-detection.
				slog.Debug("bypassing guardrail: failed to parse JSON payload",
					"error", err)
				next.ServeHTTP(w, r)
				return
			}

			// ----------------------------------------------------------------
			// 4. Streaming block
			// ----------------------------------------------------------------
			if req.Stream {
				ihttp.WriteJSONError(w, http.StatusNotImplemented,
					"STREAMING_NOT_SUPPORTED",
					"v1 does not support streaming; set stream: false")
				return
			}

			// ----------------------------------------------------------------
			// 5. Extract prompt and run the breaker.
			//    tenantID and agentID were resolved above for span attributes.
			// ----------------------------------------------------------------
			prompt := lastUserMessage(req)
			if prompt != "" {
				normalized := guardrail.Normalize(prompt)
				h := guardrail.Hash(normalized)

				if breaker.Check(tenantID, agentID, h) {
					span.AddEvent("loop_detected", trace.WithAttributes(
						attribute.String("prompt_hash", h),
					))
					slog.Warn("guardrail: loop detected",
						"tenant_id", tenantID,
						"agent_id", agentID,
						"prompt_hash", h,
					)
					ihttp.WriteJSONError(w, http.StatusTooManyRequests,
						"LOOP_DETECTED",
						"repeated identical prompt detected; request blocked")
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

// lastUserMessage returns the content of the last message with role "user"
// in the messages array, or an empty string if none exists.
func lastUserMessage(req openAIRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if strings.EqualFold(req.Messages[i].Role, "user") {
			return req.Messages[i].Content
		}
	}
	return ""
}

// isJSONContentType reports whether the request declares a JSON Content-Type.
func isJSONContentType(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "application/json")
}
