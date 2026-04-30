//go:build !goexperiment.runtimesecret

// Package secwipe wraps runtime/secret.Do so that the rest of the codebase
// can call secwipe.Do without caring whether the experiment is enabled.
// When built WITHOUT GOEXPERIMENT=runtimesecret this stub simply invokes f
// directly; no secure erasure is performed.
package secwipe

// Do invokes f directly. Build with GOEXPERIMENT=runtimesecret to enable
// secure stack/register erasure via runtime/secret.
func Do(f func()) { f() }
