// Package config loads, validates, and indexes the agentmesh YAML
// configuration file into a LoadedConfig value ready for use by the rest of
// the process.
//
// The loader applies four transformations in order:
//
//  1. YAML unmarshal into api/v1.Config.
//  2. Struct validation via go-playground/validator (required fields, URL
//     format, numeric ranges, enum values).
//  3. Default injection for Duration fields left at zero after unmarshalling.
//  4. Tenant map construction: two read-only lookup maps are built and all
//     sensitive key material is redacted from the embedded Config so that
//     the Config struct can be safely passed to loggers and tracers without
//     risk of credential leakage.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v3"

	v1 "agentmesh/api/v1"
)

// LoadedConfig is the fully validated, tenant-indexed configuration produced by
// LoadFromReader. Sensitive key material has been redacted from the embedded
// Config; use UpstreamKeyMap to retrieve upstream credentials.
type LoadedConfig struct {
	// Config is a pointer to the validated root configuration. All tenant
	// APIKey and UpstreamAPIKey fields inside it have been replaced with
	// "[REDACTED]".
	Config *v1.Config

	// TenantMap indexes tenants by their original (pre-redaction) APIKey so
	// that an incoming request key can be looked up in O(1).
	TenantMap map[string]*v1.TenantConfig

	// UpstreamKeyMap maps TenantID → real UpstreamAPIKey so that the proxy
	// can authenticate to the upstream LLM endpoint.
	UpstreamKeyMap map[string]string
}

// defaultNetworkTimeout is applied to network Duration fields (e.g.
// BudgetConfig.RequestTimeout) that are still zero after YAML unmarshalling.
const defaultNetworkTimeout = 2 * time.Second

// defaultCacheTTL is the fallback TTL applied to CacheConfig.TTL when the
// operator does not set it. 24 hours gives a sensible rolling window for
// semantic-cache entries without manual eviction.
const defaultCacheTTL = 24 * time.Hour

// Load opens the YAML file at path and delegates to LoadFromReader.
func Load(path string) (*LoadedConfig, error) {
	f, err := os.Open(path) // #nosec G304 — path is supplied by the operator via CLI flag
	if err != nil {
		return nil, fmt.Errorf("opening config file %q: %w", path, err)
	}
	defer f.Close()
	return LoadFromReader(f)
}

// LoadFromReader parses, validates and indexes an agentmesh v1 configuration
// from r. It returns an error if:
//   - r is empty
//   - the YAML is malformed
//   - any required field fails validation
func LoadFromReader(r io.Reader) (*LoadedConfig, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("config is empty")
	}

	var cfg v1.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config YAML: %w", err)
	}

	// Apply default network timeouts wherever a Duration field was omitted.
	if cfg.Budget.RequestTimeout.Duration == 0 {
		cfg.Budget.RequestTimeout = v1.Duration{Duration: defaultNetworkTimeout}
	}
	if cfg.Cache != nil && cfg.Cache.TTL.Duration == 0 {
		cfg.Cache.TTL = v1.Duration{Duration: defaultCacheTTL}
	}
	if cfg.Cache != nil && cfg.Cache.SimilarityThreshold <= 0 {
		cfg.Cache.SimilarityThreshold = 0.90
	}

	validate := validator.New()
	if err := validate.Struct(&cfg); err != nil {
		var ve validator.ValidationErrors
		if errors.As(err, &ve) {
			msgs := make([]string, 0, len(ve))
			for _, fe := range ve {
				msgs = append(msgs,
					fmt.Sprintf("field %q failed %q validation", fe.Namespace(), fe.Tag()),
				)
			}
			return nil, errors.New(strings.Join(msgs, "\n"))
		}
		return nil, err
	}

	lc := &LoadedConfig{
		Config:         &cfg,
		TenantMap:      make(map[string]*v1.TenantConfig, len(cfg.Tenants)),
		UpstreamKeyMap: make(map[string]string, len(cfg.Tenants)),
	}

	buildTenantMap(lc)
	return lc, nil
}

// buildTenantMap reads each tenant's credentials, stores the upstream key in
// UpstreamKeyMap, redacts both key fields on the struct (preventing accidental
// logging), and indexes the struct pointer in TenantMap under the original
// inbound API key for O(1) per-request lookup.
//
// Note on security posture: Go strings are heap-allocated and immutable, so
// setting a field to "[REDACTED]" only updates the struct header; the original
// backing bytes remain on the heap until GC. This is an accepted v1 tradeoff
// per ADR-002. The primary threat model addressed here is preventing keys from
// being accidentally emitted to logs or OpenTelemetry spans.
func buildTenantMap(lc *LoadedConfig) {
	for i := range lc.Config.Tenants {
		t := &lc.Config.Tenants[i]

		tenantID := t.TenantID
		originalAPIKey := t.APIKey
		upstreamKey := t.UpstreamAPIKey

		// Persist the upstream credential, keyed by tenant ID.
		lc.UpstreamKeyMap[tenantID] = upstreamKey

		// Redact both sensitive fields so they never appear in logs or
		// OpenTelemetry spans that reference lc.Config.
		t.APIKey = "[REDACTED]"
		t.UpstreamAPIKey = "[REDACTED]"

		// Index the (now-redacted) tenant struct under the original API key.
		lc.TenantMap[originalAPIKey] = t
	}
}
