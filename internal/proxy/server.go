// Package proxy implements the AgentMesh data-plane: it authenticates inbound
// requests, assembles the middleware chain, and forwards traffic to upstream
// LLM endpoints with credential substitution.
//
// # Architecture
//
// A single Server value is created at startup by NewServer. It pre-parses each
// tenant's upstream URL and allocates one connection-pooled httputil.ReverseProxy
// per tenant so that upstream connections are reused across requests.
//
// The full request pipeline, from outermost to innermost, is:
//
//	otelhttp span → AuthMiddleware → GuardrailMiddleware → [cacheMiddleware]
//	→ budget.Middleware → HandleProxy
//
// RegisterChain wires arbitrary middleware onto the root path of the mux in
// correct wrapping order (first argument = outermost after Auth). The OTel
// span and Auth layers are always applied unconditionally.
//
// Security invariants
//
//   - Inbound API keys are never forwarded upstream; they are exchanged for
//     the tenant's real upstream credential inside HandleProxy.
//   - r.Clone is used before mutating headers so the original *http.Request
//     owned by net/http is never modified.
//   - All internal maps (tenantMap, upstreamKeyMap, proxies) are written once
//     during NewServer and are read-only for the lifetime of the server,
//     making them safe for concurrent access without locking.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	v1 "agentmesh/api/v1"
	"agentmesh/internal/config"
	"agentmesh/internal/ctxkeys"
)

// tenantProxy caches the single-host reverse proxy and the parsed upstream URL
// for one tenant. Both fields are written once at construction and are
// thereafter read-only, so no mutex is needed.
type tenantProxy struct {
	proxy  *httputil.ReverseProxy
	target *url.URL
}

// Server is the agentmesh data-plane proxy. All exported-by-reference maps are
// written exclusively during NewServer and are read-only during serving, making
// them safe for concurrent access without additional locking.
type Server struct {
	cfg            *config.LoadedConfig
	tenantMap      map[string]*v1.TenantConfig // inbound API key → tenant
	upstreamKeyMap map[string]string           // TenantID → upstream API key
	proxies        map[string]*tenantProxy     // TenantID → cached reverse proxy
	mux            *http.ServeMux
}

// NewServer constructs a ready-to-use proxy Server from lc. It pre-parses every
// tenant's upstream URL and creates a connection-pooled ReverseProxy per tenant.
func NewServer(lc *config.LoadedConfig) (*Server, error) {
	proxies := make(map[string]*tenantProxy, len(lc.Config.Tenants))

	for i := range lc.Config.Tenants {
		t := &lc.Config.Tenants[i]

		target, err := url.Parse(t.UpstreamURL)
		if err != nil {
			return nil, fmt.Errorf("tenant %q: parsing upstream URL %q: %w",
				t.TenantID, t.UpstreamURL, err)
		}

		rp := httputil.NewSingleHostReverseProxy(target)

		// Capture loop-local copies for the ErrorHandler closure.
		tenantID := t.TenantID
		upstreamURL := t.UpstreamURL
		rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("upstream request failed",
				"tenant_id", tenantID,
				"upstream_url", upstreamURL,
				"error", err,
			)
			http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		}

		proxies[t.TenantID] = &tenantProxy{
			proxy:  rp,
			target: target,
		}
	}

	s := &Server{
		cfg:            lc,
		tenantMap:      lc.TenantMap,
		upstreamKeyMap: lc.UpstreamKeyMap,
		proxies:        proxies,
		mux:            http.NewServeMux(),
	}

	return s, nil
}

// Mux returns the underlying ServeMux so callers can register additional
// routes or read its address for testing.
func (s *Server) Mux() *http.ServeMux { return s.mux }

// RegisterChain wires the full middleware pipeline onto the mux at "/".
// Call this exactly once after NewServer, passing any extra middleware
// (e.g. GuardrailMiddleware, BudgetMiddleware) that should wrap HandleProxy.
// The chain is applied in the order given, outermost first, and the result
// is wrapped in an OTel HTTP span before registration.
//
//	Example:
//	  srv.RegisterChain(
//	    srv.GuardrailMiddleware(breaker),
//	    budget.Middleware(tracker),
//	  )
func (s *Server) RegisterChain(middlewares ...func(http.Handler) http.Handler) {
	var h http.Handler = http.HandlerFunc(s.HandleProxy)
	// Apply in reverse so the first middleware in the slice is the outermost.
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	s.mux.Handle("/", otelhttp.NewHandler(s.AuthMiddleware(h), "proxy"))
}

// AuthMiddleware extracts the Bearer token from the Authorization header, looks
// up the tenant in O(1), and injects it into the request context. Requests
// with an unknown or missing token are rejected with 401 Unauthorized.
// It is exported so it can be composed externally (e.g. in the admin server).
func (s *Server) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		tenant, ok := s.tenantMap[token]
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r.WithContext(ctxkeys.WithTenant(r.Context(), tenant)))
	})
}

// HandleProxy forwards the request to the cached upstream ReverseProxy for the
// tenant found in ctx. It strips the inbound Authorization header and replaces
// it with the tenant's real upstream API key.
// It is exported so it can be registered on external muxes (e.g. admin server).
func (s *Server) HandleProxy(w http.ResponseWriter, r *http.Request) {
	tenant, ok := ctxkeys.GetTenant(r.Context())
	if !ok {
		// Should never happen: authMiddleware always injects the tenant.
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	tp, ok := s.proxies[tenant.TenantID]
	if !ok {
		slog.Error("no cached proxy for tenant", "tenant_id", tenant.TenantID)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	upstreamKey, ok := s.upstreamKeyMap[tenant.TenantID]
	if !ok {
		slog.Error("no upstream key for tenant", "tenant_id", tenant.TenantID)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Clone the request so we never mutate the original received from net/http.
	outreq := r.Clone(r.Context())

	// Replace the inbound caller's API key with the real upstream credential.
	outreq.Header.Del("Authorization")
	outreq.Header.Set("Authorization", "Bearer "+upstreamKey)

	// Rewrite the Host header to match the upstream so TLS SNI and virtual
	// hosting resolve correctly.
	outreq.Host = tp.target.Host

	tp.proxy.ServeHTTP(w, outreq)
}

// StartAdmin starts a minimal HTTP server on the configured admin port.
//
// It exposes two endpoints:
//
//   - GET /healthz — liveness probe: always returns 200 {"status":"ok"} while
//     the process is running. Kubernetes uses this to decide whether to restart
//     the pod.
//
//   - GET /readyz  — readiness probe: calls readyFn and returns 200 on success
//     or 503 on failure. Kubernetes removes the pod from load-balancing rotation
//     when this returns non-2xx. The supplied readyFn should check any
//     dependency whose unavailability makes the proxy operationally unsafe
//     (e.g. a Redis ping when running with fail-closed budget mode).
//
// StartAdmin blocks until ctx is canceled, then drains in-flight requests with
// a 5-second graceful shutdown.
func (s *Server) StartAdmin(ctx context.Context, readyFn func(context.Context) error) error {
	mux := http.NewServeMux()

	// /healthz — liveness: always return 200 while the process is alive.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})

	// /readyz — readiness: call readyFn and surface dependency health.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		// Use an independent timeout shorter than the Kubernetes probe's
		// timeoutSeconds (3 s) so a slow dependency doesn't hold the probe
		// connection open until Kubernetes cancels it externally.
		checkCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := readyFn(checkCtx); err != nil {
			slog.Warn("readiness check failed", "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			body, _ := json.Marshal(map[string]string{"status": "unavailable", "error": err.Error()})
			_, _ = w.Write(body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})

	addr := fmt.Sprintf(":%d", s.cfg.Config.Server.AdminPort)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("admin graceful shutdown: %w", err)
	}
	return nil
}

// Start binds the proxy to the configured port, serves requests, and performs
// a graceful shutdown when ctx is canceled. It returns any listen error that
// is not http.ErrServerClosed.
func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.cfg.Config.Server.ProxyPort)
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		// Server failed before the context was canceled.
		return err
	case <-ctx.Done():
		// Context canceled: trigger graceful shutdown.
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("proxy graceful shutdown: %w", err)
	}
	return nil
}

// bearerToken extracts the token from an Authorization header carrying a Bearer
// credential. The prefix match is case-insensitive so that non-compliant
// clients sending "bearer " (lowercase) are still accepted.
// Returns an empty string if the header is absent or the scheme is not Bearer.
func bearerToken(r *http.Request) string {
	hdr := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(hdr), "bearer ") {
		return ""
	}
	return strings.TrimSpace(hdr[7:])
}
