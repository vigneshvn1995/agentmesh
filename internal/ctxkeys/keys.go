// Package ctxkeys defines the typed context keys used to propagate
// request-scoped values through the AgentMesh middleware chain.
//
// Using an unexported integer type (contextKey) as the key type prevents
// accidental collisions with keys defined in third-party packages.
// All values stored by this package are retrieved through typed accessor
// functions (WithTenant, GetTenant) so callers never interact with raw
// context.WithValue / Value calls.
package ctxkeys

import (
	"context"

	v1 "agentmesh/api/v1"
)

// contextKey is an unexported type for all context keys in this package,
// preventing collisions with keys defined in other packages.
type contextKey int

const (
	// tenantKey is the context key for a *v1.TenantConfig value.
	tenantKey contextKey = iota
)

// WithTenant returns a new context derived from ctx that carries tenant.
func WithTenant(ctx context.Context, tenant *v1.TenantConfig) context.Context {
	return context.WithValue(ctx, tenantKey, tenant)
}

// GetTenant retrieves the *v1.TenantConfig stored in ctx by WithTenant.
// The second return value is false if no tenant is present in the context.
func GetTenant(ctx context.Context) (*v1.TenantConfig, bool) {
	t, ok := ctx.Value(tenantKey).(*v1.TenantConfig)
	return t, ok
}
