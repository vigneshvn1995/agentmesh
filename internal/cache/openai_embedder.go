// Package cache — see ports.go for the package doc.
package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenAIEmbedder calls any OpenAI-compatible /v1/embeddings endpoint to
// produce dense vector embeddings for cache lookups. It works with
// api.openai.com as well as Azure OpenAI, local vLLM servers, and any other
// provider that implements the same HTTP contract.
type OpenAIEmbedder struct {
	client   *http.Client
	endpoint string
	apiKey   string
	model    string
}

// NewOpenAIEmbedder constructs an OpenAIEmbedder that calls endpoint with the
// provided API key and model name. timeout caps each individual embedding
// request; set it to match the request_timeout in BudgetConfig so a slow
// embedding service cannot hold the proxy open indefinitely.
func NewOpenAIEmbedder(endpoint, apiKey, model string, timeout time.Duration) *OpenAIEmbedder {
	return &OpenAIEmbedder{
		client:   &http.Client{Timeout: timeout},
		endpoint: endpoint,
		apiKey:   apiKey,
		model:    model,
	}
}

// embedRequest is the minimal body sent to the /v1/embeddings endpoint.
// Only the fields the API requires are included to keep allocations small.
type embedRequest struct {
	Input string `json:"input"`
	Model string `json:"model"`
}

// embedResponse captures only the fields AgentMesh uses. OpenAI also returns
// "object", "usage", and per-object "index"/"object" fields which are
// intentionally omitted so the decoder skips them without allocating.
type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed sends text to the configured embeddings endpoint and returns the
// resulting float32 vector. The provided ctx is forwarded to the outbound
// HTTP call so that client-side cancellation or deadline propagation abort
// the embedding request immediately rather than waiting for the server.
func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{Input: text, Model: e.model})
	if err != nil {
		return nil, fmt.Errorf("cache.OpenAIEmbedder.Embed: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("cache.OpenAIEmbedder.Embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cache.OpenAIEmbedder.Embed: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Read up to 512 bytes of the error body for diagnostics without
		// allocating for the full (potentially large) error payload.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("cache.OpenAIEmbedder.Embed: upstream %d: %s",
			resp.StatusCode, snippet)
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("cache.OpenAIEmbedder.Embed: decode response: %w", err)
	}

	if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("cache.OpenAIEmbedder.Embed: empty embedding in response")
	}

	return result.Data[0].Embedding, nil
}
