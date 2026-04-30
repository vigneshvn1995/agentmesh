package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "agentmesh/api/v1"
	"agentmesh/internal/config"
)

// Credentials used throughout the test suite.
const (
	testTenantID    = "tenant-test"
	testInboundKey  = "inbound-key-abc"       // bearer token sent by callers to agentmesh
	testUpstreamKey = "real-upstream-key-xyz" // credential that must reach the upstream
)

// newTestLoadedConfig builds a minimal *config.LoadedConfig that mirrors the
// state produced by config.LoadFromReader + buildSecureTenantMap:
//   - Config.Tenants has TenantID and UpstreamURL populated; APIKey and
//     UpstreamAPIKey are already "[REDACTED]".
//   - TenantMap is keyed by the original inbound API key.
//   - UpstreamKeyMap holds the real upstream credential, keyed by TenantID.
//
// upstreamURL is the URL the reverse proxy will forward requests to.
func newTestLoadedConfig(upstreamURL string) *config.LoadedConfig {
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
	}
	return &config.LoadedConfig{
		Config: cfg,
		// TenantMap is keyed by the original (pre-redaction) inbound API key,
		// and the value points into the Tenants slice — matching loader behaviour.
		TenantMap: map[string]*v1.TenantConfig{
			testInboundKey: &cfg.Tenants[0],
		},
		UpstreamKeyMap: map[string]string{
			testTenantID: testUpstreamKey,
		},
	}
}

// mustNewServer is a test helper that calls NewServer, wires the basic
// middleware chain (AuthMiddleware → HandleProxy), and fails the test on error.
func mustNewServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	srv, err := NewServer(newTestLoadedConfig(upstreamURL))
	if err != nil {
		t.Fatalf("NewServer(%q): %v", upstreamURL, err)
	}
	// No extra middleware for unit tests; RegisterChain wraps AuthMiddleware
	// around HandleProxy and registers on the mux.
	srv.RegisterChain()
	return srv
}

// TestProxyServer is the main test suite for the proxy Server.
// Every subtest is parallel and uses httptest.NewRecorder — no TCP listeners
// are started for the proxy itself, so there are no server-closure races.
func TestProxyServer(t *testing.T) {
	t.Parallel()

	// --- table-driven cases ------------------------------------------------
	// These cases do not need a live upstream: auth failures are handled before
	// the proxy is invoked, and "upstream unavailable" uses http://127.0.0.1:1
	// (port 1 refuses connections immediately, giving a reliable 502 without
	// ever starting a goroutine that touches a shared server variable).
	tableTests := []struct {
		name        string
		upstreamURL string
		buildReq    func() *http.Request
		wantCode    int
		wantBody    string
	}{
		{
			name:        "missing auth header returns 401",
			upstreamURL: "http://127.0.0.1:1",
			buildReq: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
			},
			wantCode: http.StatusUnauthorized,
			wantBody: "Unauthorized",
		},
		{
			name:        "invalid API key returns 401",
			upstreamURL: "http://127.0.0.1:1",
			buildReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
				req.Header.Set("Authorization", "Bearer wrong-key-000")
				return req
			},
			wantCode: http.StatusUnauthorized,
			wantBody: "Unauthorized",
		},
		{
			// Port 1 on the loopback immediately refuses the TCP connection;
			// the ReverseProxy ErrorHandler catches the dial error and writes 502.
			name:        "upstream unavailable returns 502",
			upstreamURL: "http://127.0.0.1:1",
			buildReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
				req.Header.Set("Authorization", "Bearer "+testInboundKey)
				return req
			},
			wantCode: http.StatusBadGateway,
			wantBody: "Bad Gateway",
		},
	}

	for _, tc := range tableTests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := mustNewServer(t, tc.upstreamURL)
			rec := httptest.NewRecorder()
			srv.Mux().ServeHTTP(rec, tc.buildReq())

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tc.wantBody)
			}
		})
	}

	// --- valid proxy request (live mock upstream) --------------------------
	// This subtest is kept outside the table because it needs its own
	// httptest.Server whose URL is known only at runtime.
	t.Run("valid proxy request swaps credential and reaches upstream", func(t *testing.T) {
		t.Parallel()

		// upstream is a mock LLM API. It must:
		//   1. Receive the real upstream key  — not the inbound agentmesh key.
		//   2. Return 200 OK so we can confirm end-to-end success.
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("Authorization")

			// Critical leak check: the caller's inbound key must NEVER reach
			// the upstream. If it does, agentmesh has forwarded raw credentials.
			if got == "Bearer "+testInboundKey {
				t.Errorf("CREDENTIAL LEAK: upstream received the inbound agentmesh API key %q", testInboundKey)
			}

			// The upstream must receive exactly the real upstream credential.
			want := "Bearer " + testUpstreamKey
			if got != want {
				t.Errorf("upstream Authorization = %q, want %q", got, want)
			}

			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		srv := mustNewServer(t, upstream.URL)

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
		req.Header.Set("Authorization", "Bearer "+testInboundKey)
		rec := httptest.NewRecorder()

		srv.Mux().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
		}
	})
}
