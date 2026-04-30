// Package cache implements semantic response caching for AgentMesh using a
// vector database (Qdrant) and an embedding model (OpenAI text-embedding-3-small
// by default).
//
// # Architecture
//
// When an inbound request arrives, the cache middleware:
//
//  1. Extracts the last user-role message from the request body.
//  2. Calls Embedder.Embed to convert the prompt into a dense float32 vector.
//  3. Calls VectorStore.Search to find the nearest cached response within the
//     configured cosine-similarity threshold.
//  4. On a HIT: writes the cached response directly to the client and sets
//     X-AgentMesh-Cache: HIT. The request never reaches the budget tracker or
//     upstream LLM — zero tokens are consumed.
//  5. On a MISS: forwards the request downstream, sets X-AgentMesh-Cache: MISS,
//     and asynchronously stores the successful upstream response in Qdrant for
//     future reuse.
//
// # Tenant isolation
//
// Every Qdrant query carries a must-match filter on the tenant_id payload
// field. This is enforced at the Qdrant index level, making cross-tenant
// cache contamination structurally impossible regardless of vector similarity.
//
// # Interfaces
//
// Embedder and VectorStore are defined as interfaces so that real
// implementations (OpenAIEmbedder, QdrantStore) can be swapped for test
// doubles without touching the middleware.
package cache

import (
	"context"
	"time"
)

// Entry is the value stored in and retrieved from the vector cache.
// It captures the normalised prompt, the full upstream response, and enough
// metadata to support TTL enforcement and tenant-scoped eviction.
type Entry struct {
	TenantID  string
	Prompt    string
	Response  string
	CreatedAt time.Time
}

// Embedder converts a text string into a dense vector representation.
// Implementations may call a local model, a remote embedding API, or return
// a fixed stub vector (see NoopEmbedder).
type Embedder interface {
	// Embed returns the vector embedding for text. The returned slice length
	// must be consistent across all calls to a given implementation so that
	// vectors are comparable inside the VectorStore.
	Embed(ctx context.Context, text string) ([]float32, error)
}

// VectorStore persists and retrieves CacheEntry values indexed by their
// embedding vectors. All operations are tenant-scoped so that one tenant's
// cached responses are never visible to another.
type VectorStore interface {
	// Search returns the closest cached entry whose cosine similarity to
	// embedding meets or exceeds similarityThreshold. The second return value
	// is false when no sufficiently similar entry exists.
	Search(ctx context.Context, tenantID string, embedding []float32, similarityThreshold float32) (*Entry, bool, error)

	// Store persists entry indexed by embedding. Implementations must be safe
	// for concurrent calls.
	Store(ctx context.Context, entry Entry, embedding []float32) error
}
