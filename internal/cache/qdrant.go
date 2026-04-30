// Package cache — see ports.go for the package doc.
package cache

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"
)

// Ensure QdrantStore satisfies VectorStore at compile time.
var _ VectorStore = (*QdrantStore)(nil)

// QdrantStore implements VectorStore using Qdrant as the backend.
// Each tenant's entries are stored in the same collection and isolated at
// query time via a must-match filter on the tenant_id payload field,
// making cross-tenant cache contamination structurally impossible.
type QdrantStore struct {
	client         *qdrant.Client
	collectionName string
	// ttl is the maximum age of a cached entry. Zero means no TTL enforcement.
	ttl time.Duration
}

// NewQdrantStore constructs a QdrantStore backed by the Qdrant instance at
// endpoint. apiKey is sent on every RPC as a Bearer token; pass an empty
// string for unauthenticated local instances. ttl sets the maximum age of a
// cached entry — entries older than ttl are treated as misses. Pass zero to
// disable age-based eviction (rely on Qdrant's native collection TTL instead).
//
// NewQdrantStore performs a collection existence check at startup so that a
// misconfigured collection name surfaces immediately as a fatal error rather
// than silently failing on the first cache write.
func NewQdrantStore(endpoint, apiKey, collectionName string, ttl time.Duration) (*QdrantStore, error) {
	client, err := qdrant.NewClient(&qdrant.Config{
		Host:   endpoint,
		APIKey: apiKey,
		UseTLS: apiKey != "", // only enable TLS when auth is in use
	})
	if err != nil {
		return nil, fmt.Errorf("cache.NewQdrantStore: %w", err)
	}

	// Validate the collection exists before the process finishes startup.
	// A 5-second timeout prevents indefinite hanging when Qdrant is unreachable.
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer checkCancel()
	exists, err := client.CollectionExists(checkCtx, collectionName)
	if err != nil {
		return nil, fmt.Errorf("cache.NewQdrantStore: could not check collection %q: %w", collectionName, err)
	}
	if !exists {
		return nil, fmt.Errorf("cache.NewQdrantStore: collection %q does not exist in Qdrant; create it before starting agentmesh", collectionName)
	}

	return &QdrantStore{
		client:         client,
		collectionName: collectionName,
		ttl:            ttl,
	}, nil
}

// payloadKey constants keep the field names consistent between Store and Search.
const (
	payloadTenantID  = "tenant_id"
	payloadPrompt    = "prompt"
	payloadResponse  = "response"
	payloadCreatedAt = "created_at"
)

// Store persists entry in the Qdrant collection indexed by embedding.
//
// A deterministic UUIDv5 derived from (tenantID + prompt) is used as the
// point ID so that upserting the same prompt twice is idempotent — Qdrant
// will overwrite rather than create a duplicate.
func (q *QdrantStore) Store(ctx context.Context, entry CacheEntry, embedding []float32) error {
	// Deterministic ID: same tenant + prompt always maps to the same UUID.
	pointID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(entry.TenantID+entry.Prompt))

	_, err := q.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: q.collectionName,
		Points: []*qdrant.PointStruct{
			{
				Id:      qdrant.NewID(pointID.String()),
				Vectors: qdrant.NewVectorsDense(embedding),
				Payload: qdrant.NewValueMap(map[string]any{
					payloadTenantID:  entry.TenantID,
					payloadPrompt:    entry.Prompt,
					payloadResponse:  entry.Response,
					payloadCreatedAt: entry.CreatedAt.UTC().Format(time.RFC3339),
				}),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("cache.QdrantStore.Store: %w", err)
	}

	slog.Debug("cache: stored entry",
		"tenant_id", entry.TenantID,
		"point_id", pointID,
		"embedding_len", len(embedding),
	)
	return nil
}

// Search returns the nearest cached entry whose cosine similarity to embedding
// meets or exceeds similarityThreshold, filtered strictly to tenantID.
//
// SECURITY: The must-match filter on tenant_id is not optional. Removing it
// would allow one tenant to receive another tenant's cached LLM responses.
func (q *QdrantStore) Search(ctx context.Context, tenantID string, embedding []float32, similarityThreshold float32) (*CacheEntry, bool, error) {
	limit := uint64(1)
	results, err := q.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: q.collectionName,
		Query:          qdrant.NewQueryDense(embedding),
		Filter: &qdrant.Filter{
			Must: []*qdrant.Condition{
				qdrant.NewMatchKeyword(payloadTenantID, tenantID),
			},
		},
		Limit:       &limit,
		WithPayload: qdrant.NewWithPayload(true),
	})
	if err != nil {
		return nil, false, fmt.Errorf("cache.QdrantStore.Search: %w", err)
	}

	if len(results) == 0 || results[0].GetScore() < similarityThreshold {
		slog.Debug("cache: miss",
			"tenant_id", tenantID,
			"threshold", similarityThreshold,
		)
		return nil, false, nil
	}

	hit := results[0]
	payload := hit.GetPayload()

	// Safely extract string values; GetStringValue() returns "" on nil.
	createdAtRaw := payload[payloadCreatedAt].GetStringValue()
	createdAt, err := time.Parse(time.RFC3339, createdAtRaw)
	if err != nil {
		// Malformed timestamp — treat as a miss rather than a hard error.
		slog.Warn("cache: ignoring entry with unparseable created_at",
			"tenant_id", tenantID,
			"raw", createdAtRaw,
			"error", err,
		)
		return nil, false, nil
	}

	entry := &CacheEntry{
		TenantID:  payload[payloadTenantID].GetStringValue(),
		Prompt:    payload[payloadPrompt].GetStringValue(),
		Response:  payload[payloadResponse].GetStringValue(),
		CreatedAt: createdAt,
	}

	// Enforce the configured TTL in-process. Although Qdrant supports native
	// collection-level TTL, we also check here so that a custom ttl set in
	// CacheConfig is always respected regardless of the Qdrant configuration.
	if q.ttl > 0 && time.Since(entry.CreatedAt) > q.ttl {
		slog.Debug("cache: entry expired by TTL, treating as miss",
			"tenant_id", tenantID,
			"ttl", q.ttl,
			"age", time.Since(entry.CreatedAt),
		)
		return nil, false, nil
	}

	slog.Debug("cache: hit",
		"tenant_id", tenantID,
		"score", hit.GetScore(),
		"point_id", hit.GetId(),
	)
	return entry, true, nil
}
