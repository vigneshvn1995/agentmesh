package v1

import (
	"fmt"
	"time"
)

// Duration is a wrapper around time.Duration that supports YAML unmarshalling
// from human-readable strings (e.g. "15s", "1m30s").
type Duration struct {
	time.Duration
}

// UnmarshalYAML implements yaml.Unmarshaler so Duration fields can be set from
// strings like "15s" in YAML configuration files.
func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var raw string
	if err := unmarshal(&raw); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	d.Duration = parsed
	return nil
}

// Config is the root configuration struct for agentmesh.
type Config struct {
	// Version must be exactly "v1".
	Version string `yaml:"version" validate:"required,eq=v1"`

	Server     ServerConfig    `yaml:"server"     validate:"required"`
	Tenants    []TenantConfig  `yaml:"tenants"    validate:"required,dive"`
	Guardrails GuardrailConfig `yaml:"guardrails" validate:"required"`
	Budget     BudgetConfig    `yaml:"budget"     validate:"required"`
	Redis      RedisConfig     `yaml:"redis"      validate:"required"`

	// Cache is optional.
	Cache *CacheConfig `yaml:"cache"`
}

// ServerConfig holds the ports the proxy and admin servers listen on.
type ServerConfig struct {
	ProxyPort int `yaml:"proxy_port" validate:"required,min=1,max=65535"`
	AdminPort int `yaml:"admin_port" validate:"required,min=1,max=65535"`
}

// TenantConfig describes a single tenant and its upstream LLM endpoint.
type TenantConfig struct {
	TenantID       string `yaml:"tenant_id"        validate:"required"`
	APIKey         string `yaml:"api_key"          validate:"required"`
	UpstreamURL    string `yaml:"upstream_url"     validate:"required,url"`
	UpstreamAPIKey string `yaml:"upstream_api_key" validate:"required"`
}

// LoopDetectionConfig holds the sliding-window parameters for the circuit
// breaker that detects prompt-loop abuse.
type LoopDetectionConfig struct {
	// WindowSize is the time window over which identical prompt hashes are
	// counted. Defaults to 5 minutes when unset.
	WindowSize Duration `yaml:"window_size"`

	// MaxIdenticalHash is the number of identical prompt hashes allowed within
	// WindowSize before the circuit breaker trips with 429. Defaults to 3.
	MaxIdenticalHash int `yaml:"max_identical_hash" validate:"omitempty,min=1"`
}

// GuardrailConfig holds guardrail policy settings.
type GuardrailConfig struct {
	Enabled         bool                `yaml:"enabled"`
	BlockedKeywords []string            `yaml:"blocked_keywords"`
	MaxTokens       int                 `yaml:"max_tokens"       validate:"omitempty,min=1"`
	LoopDetection   LoopDetectionConfig `yaml:"loop_detection"`
}

// BudgetConfig defines spending / rate-limit budgets.
type BudgetConfig struct {
	PerAgentDailyUSD  float64  `yaml:"per_agent_daily_usd"  validate:"required,gt=0"`
	PerTenantDailyUSD float64  `yaml:"per_tenant_daily_usd" validate:"required,gt=0"`
	RequestTimeout    Duration `yaml:"request_timeout"`

	// TokensPerUSD is the conversion rate used when translating USD budgets
	// into token counts. Defaults to 1000 (1 USD ≈ 1 000 tokens) when unset.
	// Adjust this to match the model pricing for your deployment.
	TokensPerUSD float64 `yaml:"tokens_per_usd" validate:"omitempty,gt=0"`
}

// RedisFailureMode controls the behaviour of budget enforcement when Redis is
// unavailable.
type RedisFailureMode string

const (
	// FailOpen allows requests to proceed when Redis cannot be reached.
	// Use for high-availability deployments where a brief budget overrun is
	// preferable to a service outage.
	FailOpen RedisFailureMode = "fail-open"

	// FailClosed blocks requests when Redis cannot be reached.
	// Use for strict budget enforcement where an overrun is never acceptable.
	FailClosed RedisFailureMode = "fail-closed"
)

// RedisConfig holds connection details for the Redis instance used by agentmesh.
type RedisConfig struct {
	Address     string           `yaml:"address"      validate:"required"`
	Password    string           `yaml:"password"`
	DB          int              `yaml:"db"           validate:"min=0"`
	PoolSize    int              `yaml:"pool_size"    validate:"omitempty,min=1"`
	FailureMode RedisFailureMode `yaml:"failure_mode"`
}

// CacheConfig is an optional semantic / response cache configuration.
type CacheConfig struct {
	Enabled bool `yaml:"enabled"`

	// TTL is the maximum age of a cached entry. Entries older than this are
	// treated as misses and the request is forwarded to the upstream.
	// Defaults to 24h when unset.
	TTL Duration `yaml:"ttl"`

	// MaxSize is reserved for a future collection size cap.
	MaxSize int `yaml:"max_size" validate:"omitempty,min=1"`

	// SimilarityThreshold is the minimum cosine similarity score required for a
	// Qdrant result to be considered a cache hit. Values closer to 1.0 require a
	// near-exact match; ~0.85–0.90 catches semantically equivalent paraphrases.
	// Defaults to 0.90 when unset. Must be in the range (0, 1].
	SimilarityThreshold float32 `yaml:"similarity_threshold" validate:"omitempty,gt=0,lte=1"`
}
