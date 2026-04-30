//go:build ignore

// This file is excluded from all builds (including go test) by the
// //go:build ignore constraint. It is kept as a reference implementation of
// the Embedder interface for local experimentation. Copy the type inline into
// your test file rather than importing from here.

package cache

import "context"

// NoopEmbedder is a placeholder Embedder that returns a fixed three-dimensional
// vector for every input. It satisfies the Embedder interface so that the full
// middleware pipeline can be wired end-to-end before a real embedding model is
// available.
type NoopEmbedder struct{}

// Embed ignores text and returns a constant stub vector.
func (NoopEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}
