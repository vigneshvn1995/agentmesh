package config

import (
	"strings"
	"testing"
)

// validYAML is a minimal but fully valid agentmesh v1 configuration.
const validYAML = `
version: v1
server:
  proxy_port: 8080
  admin_port: 9090
tenants:
  - tenant_id: tenant-acme
    api_key: incoming-api-key-abc
    upstream_url: https://api.openai.com/v1
    upstream_api_key: real-upstream-secret-xyz
guardrails:
  enabled: true
budget:
  per_agent_daily_usd: 100.0
  per_tenant_daily_usd: 10.0
  request_timeout: 5s
redis:
  address: "localhost:6379"
`

// emptyYAML represents a completely empty file payload.
const emptyYAML = ``

// missingVersionYAML omits the required version field.
const missingVersionYAML = `
server:
  proxy_port: 8080
  admin_port: 9090
tenants:
  - tenant_id: tenant-acme
    api_key: incoming-api-key-abc
    upstream_url: https://api.openai.com/v1
    upstream_api_key: real-upstream-secret-xyz
guardrails:
  enabled: true
budget:
  per_agent_daily_usd: 100.0
  per_tenant_daily_usd: 10.0
redis:
  address: "localhost:6379"
`

// invalidPortYAML sets proxy_port to a value above the max of 65535.
const invalidPortYAML = `
version: v1
server:
  proxy_port: 99999
  admin_port: 9090
tenants:
  - tenant_id: tenant-acme
    api_key: incoming-api-key-abc
    upstream_url: https://api.openai.com/v1
    upstream_api_key: real-upstream-secret-xyz
guardrails:
  enabled: true
budget:
  per_agent_daily_usd: 100.0
  per_tenant_daily_usd: 10.0
redis:
  address: "localhost:6379"
`

func TestLoadFromReader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		yaml string

		// wantErr, when true, expects LoadFromReader to return a non-nil error
		// whose message contains wantErrSubstr.
		wantErr       bool
		wantErrSubstr string

		// When wantErr is false these callbacks are invoked against the result.
		checkFn func(t *testing.T, lc *LoadedConfig)
	}{
		{
			name: "valid config redacts keys and preserves upstream key",
			yaml: validYAML,
			checkFn: func(t *testing.T, lc *LoadedConfig) {
				t.Helper()

				if lc.Config == nil {
					t.Fatal("expected non-nil Config")
				}

				// There must be exactly one tenant in the slice.
				if got := len(lc.Config.Tenants); got != 1 {
					t.Fatalf("expected 1 tenant, got %d", got)
				}

				tenant := &lc.Config.Tenants[0]

				// Both sensitive fields must be wiped on the struct.
				if tenant.APIKey != "[REDACTED]" {
					t.Errorf("APIKey = %q, want \"[REDACTED]\"", tenant.APIKey)
				}
				if tenant.UpstreamAPIKey != "[REDACTED]" {
					t.Errorf("UpstreamAPIKey = %q, want \"[REDACTED]\"", tenant.UpstreamAPIKey)
				}

				// TenantMap must be keyed by the original (pre-redaction) API key.
				const originalAPIKey = "incoming-api-key-abc"
				if _, ok := lc.TenantMap[originalAPIKey]; !ok {
					t.Errorf("TenantMap missing entry for original API key %q", originalAPIKey)
				}

				// UpstreamKeyMap must contain the real upstream credential.
				const tenantID = "tenant-acme"
				got, ok := lc.UpstreamKeyMap[tenantID]
				if !ok {
					t.Errorf("UpstreamKeyMap missing entry for tenant %q", tenantID)
				}
				if got != "real-upstream-secret-xyz" {
					t.Errorf("UpstreamKeyMap[%q] = %q, want \"real-upstream-secret-xyz\"", tenantID, got)
				}
			},
		},
		{
			name:          "empty file returns error",
			yaml:          emptyYAML,
			wantErr:       true,
			wantErrSubstr: "empty",
		},
		{
			name:          "missing version triggers validator",
			yaml:          missingVersionYAML,
			wantErr:       true,
			wantErrSubstr: "Version",
		},
		{
			name:          "proxy port out of range triggers validator",
			yaml:          invalidPortYAML,
			wantErr:       true,
			wantErrSubstr: "ProxyPort",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lc, err := LoadFromReader(strings.NewReader(tc.yaml))

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrSubstr)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErrSubstr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.checkFn != nil {
				tc.checkFn(t, lc)
			}
		})
	}
}
