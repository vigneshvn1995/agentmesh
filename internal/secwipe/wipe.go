//go:build goexperiment.runtimesecret

// Package secwipe wraps runtime/secret.Do so that the rest of the codebase
// can call secwipe.Do without caring whether the experiment is enabled.
package secwipe

import "runtime/secret"

// Do invokes f and ensures that any stack memory and registers used by f are
// securely erased before returning, courtesy of runtime/secret.
func Do(f func()) { secret.Do(f) }
