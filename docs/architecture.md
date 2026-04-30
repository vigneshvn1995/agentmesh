# AgentMesh Architecture

This document is the authoritative technical reference for the AgentMesh v1 design. It covers every subsystem in production depth: data models, concurrency guarantees, security invariants, and the precise request lifecycle from TCP accept to upstream response.

---

## Table of Contents

1. [System Overview](#1-system-overview)
2. [Package Dependency Graph](#2-package-dependency-graph)
3. [Full Request Lifecycle](#3-full-request-lifecycle)
4. [Auth & Tenant Isolation](#4-auth--tenant-isolation)
5. [Guardrail Layer](#5-guardrail-layer)
6. [Semantic Cache Layer](#6-semantic-cache-layer)
7. [Budget Enforcement Layer](#7-budget-enforcement-layer)
8. [Reverse Proxy Layer](#8-reverse-proxy-layer)
9. [OpenTelemetry Instrumentation](#9-opentelemetry-instrumentation)
10. [Configuration & Startup](#10-configuration--startup)
11. [Memory & Allocation Strategy](#11-memory--allocation-strategy)
12. [Security Model](#12-security-model)
13. [V1 Constraints & V2 Roadmap](#13-v1-constraints--v2-roadmap)

---

## 1. System Overview

AgentMesh is a **middleware fabric** — not merely a reverse proxy. A reverse proxy forwards traffic. AgentMesh intercepts, analyses, guards, caches, accounts, and only then forwards — or answers from cache without ever touching the upstream.

```mermaid
graph TB
    subgraph Callers
        A1[Agent / App 1<br/>Bearer am_key_acme]
        A2[Agent / App 2<br/>Bearer am_key_beta]
    end

    subgraph AgentMesh Process
        direction TB
        AUTH[AuthMiddleware<br/>O(1) tenant lookup]
        OTEL[OTel Span<br/>otelhttp.NewHandler]
        GRD[GuardrailMiddleware<br/>body limit · stream block · loop breaker]
        CACHE[CacheMiddleware<br/>embed → search → HIT or MISS]
        BUDGET[BudgetMiddleware<br/>pre-flight check · post-flight record]
        PROXY[HandleProxy<br/>credential swap · ReverseProxy]
    end

    subgraph External Services
        REDIS[(Redis<br/>budget counters)]
        QDRANT[(Qdrant<br/>vector index)]
        EMBED[Embeddings API<br/>text-embedding-3-small]
        LLM[Upstream LLM<br/>OpenAI / Azure / vLLM]
        OTLP[OTLP Collector<br/>traces]
    end

    A1 -->|POST /v1/chat/completions| AUTH
    A2 -->|POST /v1/chat/completions| AUTH
    AUTH --> OTEL --> GRD --> CACHE --> BUDGET --> PROXY

    CACHE -->|Search| QDRANT
    CACHE -->|Embed| EMBED
    CACHE -->|async Store on miss| QDRANT
    BUDGET -->|IsBudgetExceeded| REDIS
    BUDGET -->|async RecordUsage| REDIS
    PROXY -->|Bearer real-key| LLM
    AUTH -.->|traces| OTLP
```

The middleware chain is assembled once at startup by `RegisterChain` and is **immutable** during serving. Every layer is a plain `func(http.Handler) http.Handler` value — Go's standard middleware contract.

---

## 2. Package Dependency Graph

```mermaid
graph LR
    cmd["cmd/agentmesh<br/>(main)"]

    subgraph internal
        config["internal/config"]
        proxy["internal/proxy"]
        budget["internal/budget"]
        cache["internal/cache"]
        guardrail["internal/guardrail"]
        httputil["internal/httputil"]
        ctxkeys["internal/ctxkeys"]
        telemetry["internal/telemetry"]
        secwipe["internal/secwipe"]
    end

    apiv1["api/v1"]

    cmd --> config
    cmd --> proxy
    cmd --> budget
    cmd --> cache
    cmd --> guardrail
    cmd --> telemetry

    config --> apiv1
    proxy --> apiv1
    proxy --> config
    proxy --> ctxkeys
    proxy --> httputil
    proxy --> guardrail
    budget --> apiv1
    budget --> ctxkeys
    budget --> httputil
    cache --> ctxkeys
    cache --> httputil
    telemetry --> apiv1
```

**Dependency rules enforced by the compiler:**
- `internal/ctxkeys` has zero internal imports — it is the foundation layer
- `internal/httputil` has zero internal imports
- `internal/guardrail` has zero internal imports
- `internal/cache` depends only on `ctxkeys` and `httputil` — it cannot import `proxy` or `budget`
- `internal/budget` depends only on `apiv1`, `ctxkeys`, and `httputil`

This layering ensures that the cache middleware can be tested in full isolation without pulling in the proxy or budget subsystems.

---

## 3. Full Request Lifecycle

### 3.1 Cache Miss (normal request)

```mermaid
sequenceDiagram
    autonumber
    participant Agent
    participant OTel as OTel Span
    participant Auth as AuthMiddleware
    participant GRD as GuardrailMiddleware
    participant CACHE as CacheMiddleware
    participant BUDGET as BudgetMiddleware
    participant PROXY as HandleProxy
    participant EMBED as Embeddings API
    participant QDRANT as Qdrant
    participant REDIS as Redis
    participant LLM as Upstream LLM

    Agent->>OTel: POST /v1/chat/completions<br/>Authorization: Bearer am_key_acme<br/>X-Agent-ID: agent-001
    OTel->>Auth: begin span "proxy"

    Auth->>Auth: bearerToken() — case-insensitive CutPrefix
    Auth->>Auth: O(1) map lookup in tenantMap
    Auth->>Auth: ctxkeys.WithTenant(ctx, tenant)
    Auth->>GRD: next.ServeHTTP — tenant in ctx

    GRD->>GRD: nil body guard → io.LimitReader(body, 1MiB+1)
    GRD->>GRD: io.ReadAll — enforce 1 MiB limit
    GRD->>GRD: r.Body = io.NopCloser(bytes.NewReader) — restore body
    GRD->>GRD: isJSONContentType check
    GRD->>GRD: json.Unmarshal → openAIRequest{Stream, Messages}
    GRD->>GRD: req.Stream == true → 501 STREAMING_NOT_SUPPORTED

    note over GRD: Loop detection
    GRD->>GRD: lastUserMessage(req) → extract prompt
    GRD->>GRD: guardrail.Normalize(prompt) — lowercase, strip UUIDs/dates/punct, strip filler prefixes
    GRD->>GRD: guardrail.Hash(normalized) — SHA-256 from sync.Pool
    GRD->>GRD: breaker.Check(tenantID, agentID, hash)

    alt identical hash count >= limit within window
        GRD-->>Agent: 429 LOOP_DETECTED
    else
        GRD->>CACHE: next.ServeHTTP
    end

    CACHE->>CACHE: ctxkeys.GetTenant → tenantID
    CACHE->>CACHE: isJSONContentType check
    CACHE->>CACHE: io.ReadAll + restore body
    CACHE->>CACHE: json.Unmarshal → lastUserContent(req)

    CACHE->>EMBED: POST /v1/embeddings<br/>{input: prompt, model: text-embedding-3-small}
    EMBED-->>CACHE: {data: [{embedding: [float32 x 1536]}]}

    CACHE->>QDRANT: Query{filter: must match tenant_id, limit: 1, with_payload: true}
    QDRANT-->>CACHE: [] (empty — cache miss)

    CACHE->>CACHE: w.Header().Set("X-AgentMesh-Cache", "MISS")
    CACHE->>CACHE: rec = ihttp.NewResponseRecorder(w)
    CACHE->>BUDGET: next.ServeHTTP(rec, r)

    BUDGET->>REDIS: GET budget:tenant:<tenantID>
    REDIS-->>BUDGET: 8500 (tokens used today)
    BUDGET->>REDIS: GET budget:agent:<agentID>
    REDIS-->>BUDGET: 200 (tokens used today)

    alt tenant or agent budget exceeded
        BUDGET-->>Agent: 402 BUDGET_EXCEEDED
    else
        BUDGET->>PROXY: next.ServeHTTP(rec, r)
    end

    PROXY->>PROXY: ctxkeys.GetTenant → tenant
    PROXY->>PROXY: r.Clone(ctx) — never mutate original
    PROXY->>PROXY: Del "Authorization" + Set "Bearer real-upstream-key"
    PROXY->>PROXY: outreq.Host = upstream.Host
    PROXY->>LLM: POST /v1/chat/completions<br/>Authorization: Bearer sk-real-key

    LLM-->>PROXY: 200 {"choices":[...],"usage":{"total_tokens":42}}
    PROXY-->>BUDGET: response written to rec

    note over BUDGET: Post-flight token accounting
    BUDGET->>BUDGET: json.Unmarshal(rec.Body()) → usage.total_tokens = 42
    BUDGET->>BUDGET: context.WithoutCancel(ctx)
    BUDGET-)REDIS: goroutine: TxPipeline INCRBY+EXPIREX (tenant + agent)

    note over CACHE: Post-flight async cache store
    CACHE->>CACHE: rec.Status() == 200 → copy body bytes
    CACHE->>CACHE: context.WithoutCancel(ctx)
    CACHE-)QDRANT: goroutine: Upsert{id: UUIDv5(tenantID+prompt), vector: embedding, payload: {tenant_id, prompt, response, created_at}}

    PROXY-->>Agent: 200 {"choices":[...],"usage":{"total_tokens":42}}
    OTel->>OTel: end span, export to OTLP collector
```

### 3.2 Cache Hit (zero-cost path)

```mermaid
sequenceDiagram
    autonumber
    participant Agent
    participant Auth as AuthMiddleware
    participant GRD as GuardrailMiddleware
    participant CACHE as CacheMiddleware
    participant BUDGET as BudgetMiddleware
    participant PROXY as HandleProxy
    participant EMBED as Embeddings API
    participant QDRANT as Qdrant

    note over BUDGET,PROXY: These layers are never reached on a cache hit

    Agent->>Auth: POST /v1/chat/completions
    Auth->>Auth: O(1) tenant lookup → inject ctx
    Auth->>GRD: next.ServeHTTP

    GRD->>GRD: body limit + loop detection (same as miss path)
    GRD->>CACHE: next.ServeHTTP (prompt not blocked)

    CACHE->>EMBED: POST /v1/embeddings {input: prompt}
    EMBED-->>CACHE: [float32 x 1536]

    CACHE->>QDRANT: Query{must: tenant_id == "acme", limit: 1}
    QDRANT-->>CACHE: [{score: 0.97, payload: {response: "..."}}]

    CACHE->>CACHE: score 0.97 >= threshold 0.90 → HIT
    CACHE->>CACHE: w.Header().Set("Content-Type", "application/json")
    CACHE->>CACHE: w.Header().Set("X-AgentMesh-Cache", "HIT")
    CACHE->>CACHE: w.WriteHeader(200)
    CACHE->>CACHE: io.WriteString(w, entry.Response)
    CACHE-->>Agent: 200 {"choices":[...]} ← from Qdrant, < 5 ms

    note over BUDGET,PROXY: Budget untouched. Redis: 0 tokens deducted.<br/>LLM: never called. Carbon: zero inference.
```

---

## 4. Auth & Tenant Isolation

### 4.1 Credential flow

```mermaid
flowchart LR
    subgraph "Inbound (agent-facing)"
        IK[am_live_acme_abc123<br/>inbound API key]
    end

    subgraph "AgentMesh Memory"
        TM["tenantMap\nmap[inboundKey → *TenantConfig"]
        UM["upstreamKeyMap\nmap[TenantID → upstreamKey"]
    end

    subgraph "Outbound (LLM-facing)"
        UK[sk-real-openai-key<br/>upstream API key]
    end

    IK -->|Bearer header| TM
    TM -->|O(1) lookup, inject ctx| UM
    UM -->|credential swap in Clone| UK

    style IK fill:#ffeeba
    style UK fill:#d4edda
    style TM fill:#cce5ff
    style UM fill:#cce5ff
```

Both maps are written **once** during `NewServer` and are read-only for the entire lifetime of the process. No mutex is needed — Go's memory model guarantees visibility of writes before a goroutine is started.

### 4.2 Key redaction

`config.buildTenantMap` performs irreversible redaction before the config is used anywhere:

```
TenantConfig.APIKey         → "[REDACTED]"
TenantConfig.UpstreamAPIKey → "[REDACTED]"
LoadedConfig.UpstreamKeyMap[TenantID] = <real key>  (never logged)
```

No sensitive credential ever appears in a `slog` call.

### 4.3 Bearer token parsing

```go
// case-insensitive: "bearer ", "Bearer ", "BEARER " all accepted
if !strings.HasPrefix(strings.ToLower(hdr), "bearer ") {
    return ""
}
return strings.TrimSpace(hdr[7:])
```

The original casing of the token itself is preserved (only the prefix is lowercased).

---

## 5. Guardrail Layer

### 5.1 Prompt normalisation pipeline

```mermaid
flowchart TD
    RAW["Raw prompt\n&quot;Please analyze this data from 2024-03-15T10:30:00Z for UUID 550e8400-e29b-41d4-a716-446655440000&quot;"]

    LOWER["1. lowercase\n&quot;please analyze this data from 2024-03-15t10:30:00z for uuid 550e8400-e29b-41d4-a716-446655440000&quot;"]

    UUID["2. strip UUIDs\n&quot;please analyze this data from 2024-03-15t10:30:00z for uuid &quot;"]

    DATE["3. strip ISO 8601\n&quot;please analyze this data from  for uuid &quot;"]

    PUNCT["4. strip punctuation [^a-z0-9\\s]\n&quot;please analyze this data from  for uuid &quot;"]

    SPACE["5. collapse whitespace\n&quot;please analyze this data from for uuid&quot;"]

    FILLER["6. strip filler prefixes (iterative TrimPrefix)\nfillerPrefixes = [please , can you , could you , ...]\n&quot;analyze this data from for uuid&quot;"]

    HASH["SHA-256 → hex string\n(sha256.New from sync.Pool)"]

    RAW --> LOWER --> UUID --> DATE --> PUNCT --> SPACE --> FILLER --> HASH
```

**Critical design constraint:** Filler stripping uses `strings.TrimPrefix` in a loop — NOT a global string replacer. This ensures that the word "please" appearing mid-sentence (e.g., "help me analyze, please") is **not** stripped.

### 5.2 Sliding-window circuit breaker

```mermaid
stateDiagram-v2
    [*] --> Closed : NewBreaker(window, limit)

    Closed --> Closed : Check() — count < limit\nappend timestamp to history[key]

    Closed --> Open : Check() — count >= limit\nreturn blocked=true

    Open --> Closed : Sweep() called (every 5 min)\nprune expired timestamps\ncount drops below limit

    note right of Closed
        history: map[compositeKey][]time.Time
        compositeKey = tenantID|agentID|promptHash
        in-place slice pruning — zero allocation
        guarded by sync.Mutex
    end note
```

The composite key `tenantID|agentID|hash` means the same agent repeatedly sending the same prompt trips the breaker, while different agents sending the same prompt do **not** interfere with each other's budgets.

---

## 6. Semantic Cache Layer

### 6.1 Component hierarchy

```mermaid
classDiagram
    class Embedder {
        <<interface>>
        +Embed(ctx, text) ([]float32, error)
    }

    class VectorStore {
        <<interface>>
        +Search(ctx, tenantID, embedding, threshold) (*CacheEntry, bool, error)
        +Store(ctx, entry, embedding) error
    }

    class CacheEntry {
        +TenantID string
        +Prompt string
        +Response string
        +CreatedAt time.Time
    }

    class NoopEmbedder {
        +Embed() []float32{0.1, 0.2, 0.3}
    }

    class OpenAIEmbedder {
        -client *http.Client
        -endpoint string
        -apiKey string
        -model string
        +Embed(ctx, text) ([]float32, error)
    }

    class QdrantStore {
        -client *qdrant.Client
        -collectionName string
        +Search(ctx, tenantID, embedding, threshold) (*CacheEntry, bool, error)
        +Store(ctx, entry, embedding) error
    }

    class Config {
        +SimilarityThreshold float32
    }

    Embedder <|.. NoopEmbedder
    Embedder <|.. OpenAIEmbedder
    VectorStore <|.. QdrantStore
    VectorStore ..> CacheEntry
```

### 6.2 Qdrant point schema

Each cached prompt is stored as a single Qdrant point:

| Field | Type | Value |
|---|---|---|
| `id` | UUID | `UUIDv5(NameSpaceURL, tenantID+prompt)` — deterministic, idempotent |
| `vector` | `[]float32` | Output of `text-embedding-3-small` (1536 dimensions) |
| `payload.tenant_id` | string | Tenant identifier |
| `payload.prompt` | string | Original user prompt |
| `payload.response` | string | Full upstream JSON response (raw bytes) |
| `payload.created_at` | string | RFC3339 UTC timestamp |

**Idempotency:** UUIDv5 is derived deterministically from `tenantID+prompt`. Upserting the same prompt twice overwrites rather than duplicates — critical for correctness in concurrent deployments.

### 6.3 Tenant isolation invariant

```mermaid
flowchart LR
    subgraph "Query sent to Qdrant"
        Q["QueryPoints{\n  CollectionName: agentmesh_cache,\n  Query: NewQueryDense(embedding),\n  Filter: {\n    Must: [NewMatchKeyword(tenant_id, tenantID)]\n  },\n  Limit: 1,\n  WithPayload: true\n}"]
    end

    Q -->|MUST filter enforced by Qdrant at the index level| R["Results scoped to tenantID ONLY"]
    
    style Q fill:#fff3cd
    style R fill:#d4edda
```

The `Must` filter is not advisory — Qdrant evaluates it before scoring. It is structurally **impossible** for tenant A's cache entry to be returned in a query for tenant B. This is zero-trust isolation at the storage layer.

### 6.4 Asynchronous store with body copy safety

```mermaid
sequenceDiagram
    participant MW as CacheMiddleware goroutine
    participant BUF as ResponseRecorder buffer (sync.Pool)
    participant GOROUTINE as async store goroutine
    participant QDRANT as Qdrant

    MW->>BUF: rec = NewResponseRecorder(w)
    MW->>BUF: next.ServeHTTP(rec, r) — body written into buf
    MW->>MW: bodySnapshot = make([]byte, len(rec.Body()))\ncopy(bodySnapshot, rec.Body())
    note over MW,BUF: CRITICAL: copy happens BEFORE defer rec.Free()
    MW->>GOROUTINE: go func() { store.Store(detachedCtx, entry, embedding) }
    MW->>BUF: defer rec.Free() — buf returned to pool

    note over BUF: Pool buffer may be reused immediately for the\nnext concurrent request. Goroutine holds bodySnapshot,\nnot a reference to the pool buffer. Zero data race.

    GOROUTINE->>QDRANT: Upsert with string(bodySnapshot)
```

Without the explicit `copy`, the goroutine would hold a slice backed by the pool buffer. When `rec.Free()` returns that buffer to the pool, the next `NewResponseRecorder` call resets and reuses it — silently corrupting the goroutine's data under load.

---

## 7. Budget Enforcement Layer

### 7.1 Eventual consistency model

```mermaid
sequenceDiagram
    participant R1 as Request N
    participant R2 as Request N+1
    participant REDIS as Redis

    R1->>REDIS: GET budget:agent:agent-001 → 20 tokens
    note over R1: 20 < 25 → PASS
    R1->>LLM: forward
    LLM-->>R1: usage.total_tokens = 10
    R1-)REDIS: async TxPipeline INCRBY 10 → 30 tokens

    R2->>REDIS: GET budget:agent:agent-001
    note over R2: If async write completes before R2's preflight: 30 ≥ 25 → BLOCKED (402)\nIf async write not yet visible: 20 < 25 → PASS (one-request overrun)
```

This is the **intentional v1 tradeoff**: recording happens post-flight because the token count is not known until the upstream responds. A single request may exceed the limit by the cost of one request. V2 will pre-authorise token reservations.

### 7.2 Redis key design

```
budget:tenant:<TenantID>   →  INCRBY  +  EXPIREX 48h
budget:agent:<AgentID>     →  INCRBY  +  EXPIREX 48h
```

`EXPIREX` (set TTL only if not already set) prevents concurrent requests from resetting the 48-hour window. Combined with `MULTI/EXEC` (`TxPipeline`):

```mermaid
sequenceDiagram
    participant APP as AgentMesh
    participant REDIS as Redis

    APP->>REDIS: MULTI
    APP->>REDIS: INCRBY budget:tenant:acme 42
    APP->>REDIS: EXPIREX budget:tenant:acme 172800
    APP->>REDIS: INCRBY budget:agent:agent-001 42
    APP->>REDIS: EXPIREX budget:agent:agent-001 172800
    APP->>REDIS: EXEC

    note over REDIS: All 4 commands are atomic.\nIf connection drops, Redis auto-discards the transaction.\nNo key is left with a counter but no TTL.
```

### 7.3 Failure modes

```mermaid
flowchart TD
    A[Redis error on IsBudgetExceeded] --> B{failureMode}
    B -->|fail-open| C[slog.Warn\nAllow request through\nAvailability over correctness]
    B -->|fail-closed| D[503 REDIS_UNAVAILABLE\nBlock request\nCorrectness over availability]

    style C fill:#d4edda
    style D fill:#f8d7da
```

---

## 8. Reverse Proxy Layer

### 8.1 Request mutation safety

```mermaid
flowchart LR
    ORIG["Original *http.Request\n(owned by net/http runtime)"]
    CLONE["outreq = r.Clone(r.Context())\n(deep copy of headers, shallow copy of body)"]
    MUTATE["outreq.Header.Del Authorization\noutreq.Header.Set Authorization Bearer real-key\noutreq.Host = upstream.Host"]
    PROXY["tp.proxy.ServeHTTP(w, outreq)\n(ReverseProxy sends outreq upstream)"]

    ORIG -->|never mutated| CLONE --> MUTATE --> PROXY

    style ORIG fill:#fff3cd
    style CLONE fill:#cce5ff
```

`r.Clone` creates a shallow copy. Headers are deep-copied. The body is shared but the `GuardrailMiddleware` has already replaced it with a fresh `io.NopCloser(bytes.NewReader(...))` — both the original and clone read from the same in-memory bytes, which is safe because the body is only read once.

### 8.2 Upstream connection pool

Each tenant gets one `*httputil.ReverseProxy` instance, created at startup and reused for all requests. `ReverseProxy` internally uses `http.DefaultTransport` which maintains a persistent connection pool with:
- Keep-alive connections
- Configurable `MaxIdleConnsPerHost`
- Automatic retry on connection reset (idempotent requests)

---

## 9. OpenTelemetry Instrumentation

```mermaid
flowchart TB
    subgraph "AgentMesh process"
        HANDLER["otelhttp.NewHandler(chain, &quot;proxy&quot;)\n— span name: proxy"]
        TP["sdktrace.NewTracerProvider\nBatchSpanProcessor"]
        PROP["W3C TraceContext + Baggage\npropagation.NewCompositeTextMapPropagator"]
    end

    subgraph "Outbound"
        EXPORTER["otlptracegrpc.New\n5-second startup timeout\nreconnects automatically"]
        COLLECTOR["OTLP Collector\nlocalhost:4317 default"]
    end

    HANDLER -->|creates root span| TP
    TP --> EXPORTER --> COLLECTOR

    PROP -.->|reads/writes W3C headers| HANDLER
    HANDLER -.->|propagates trace context to| LLM["Upstream LLM\n(if it supports W3C)"]
```

**Startup resilience:** `otlptracegrpc.New` is wrapped in a `context.WithTimeout(ctx, 5s)`. If the OTLP collector is not yet ready (common in Kubernetes before the sidecar starts), AgentMesh starts normally and the SDK retries the connection in the background. Spans are buffered in memory until export succeeds.

---

## 10. Configuration & Startup

```mermaid
flowchart TD
    CLI["flag.Parse()\n-config agentmesh.yaml"]
    LOAD["config.Load(path)\n1. os.Open #nosec G304\n2. io.ReadAll\n3. yaml.Unmarshal\n4. apply defaults (2s timeout)\n5. validator.New().Struct\n6. buildTenantMap\n   — save upstream keys\n   — REDACT APIKey + UpstreamAPIKey"]

    OTEL["telemetry.InitProvider\nW3C propagator\nresource: service.name + version\nBatchSpanProcessor\n5s exporter timeout"]

    BREAKER["guardrail.NewBreaker\nwindow from config (default 5m)\nlimit from config (default 3)\nbackground Sweep goroutine every 5m"]

    REDIS["redis.NewClient\nAddr / Password / DB / PoolSize"]

    TRACKER["budget.NewTracker\ntenantLimit = PerTenantDailyUSD × TokensPerUSD\nagentLimit = PerAgentDailyUSD × TokensPerUSD\ndefaultTokensPerUSD = 1000"]

    CACHEBLOCK{{"cache.enabled?"}}
    QDRANT_INIT["cache.NewQdrantStore\nQDRANT_ENDPOINT + QDRANT_API_KEY\ncollection: agentmesh_cache\nUseTLS = apiKey != ''"]
    EMBEDDER_INIT["cache.NewOpenAIEmbedder\nhttps://api.openai.com/v1/embeddings\nOPENAI_API_KEY\nmodel: text-embedding-3-small\ntimeout: BudgetConfig.RequestTimeout"]
    CACHE_MW["cache.Middleware(store, embedder, Config{Threshold: 0.90})"]
    NOOP["no-op middleware\nfunc(next) { return next }"]

    SERVER["proxy.NewServer(lc)\nper-tenant ReverseProxy\nErrorHandler → slog.Error + 502"]

    CHAIN["srv.RegisterChain(\n  GuardrailMiddleware(breaker),\n  cacheMiddleware,\n  budget.Middleware(tracker),\n)\n— reverse-iteration wrapping\n— OTel span at outermost layer"]

    SIGNAL["signal.Notify SIGINT SIGTERM\ncancel() → srv.Shutdown(15s)"]

    START["srv.Start(ctx)\nhttp.Server{ReadHeaderTimeout: 10s}\nListenAndServe :ProxyPort"]

    CLI --> LOAD --> OTEL --> BREAKER --> REDIS --> TRACKER --> CACHEBLOCK
    CACHEBLOCK -->|yes| QDRANT_INIT --> EMBEDDER_INIT --> CACHE_MW --> SERVER
    CACHEBLOCK -->|no| NOOP --> SERVER
    SERVER --> CHAIN --> SIGNAL --> START
```

---

## 11. Memory & Allocation Strategy

AgentMesh is designed for the hot path to allocate as little as possible.

### sync.Pool usage

| Pool | Type | Location | Reuse |
|---|---|---|---|
| `bufPool` | `*bytes.Buffer` | `internal/httputil/recorder.go` | Response body capture per request |
| `hashPool` | `hash.Hash` (SHA-256) | `internal/guardrail/normalizer.go` | Prompt hashing per request |

**Pool correctness contract:** A buffer acquired from `bufPool` must be `Reset()` before use and `Put()` after — never after any goroutine holds a reference to its backing array. In `CacheMiddleware`, the body is `copy`'d into a new slice before the recorder is freed.

### In-place slice operations

The circuit breaker's `Check` and `Sweep` methods prune expired timestamps in-place:

```go
// in-place filter — reuses the backing array, zero new allocations
valid := 0
for _, t := range times {
    if now.Sub(t) < b.window {
        times[valid] = t
        valid++
    }
}
b.history[k] = times[:valid]
```

---

## 12. Security Model

### Threat matrix

| Threat | Mitigation |
|---|---|
| Credential exposure in logs | `buildTenantMap` redacts all keys to `[REDACTED]` before any logging |
| Credential exposure via config dump | Same redaction; `UpstreamKeyMap` lives only in memory |
| Cross-tenant cache poisoning | Qdrant `Must` filter on `tenant_id` — enforced at index, not application layer |
| Prompt injection via JSON | `encoding/json` marshal/unmarshal everywhere; no string concatenation for JSON construction |
| Body-based DoS | `io.LimitReader(body, 1 MiB + 1)` hard limit; rejected before any parsing |
| Slowloris | `ReadHeaderTimeout: 10s` on `http.Server` |
| Embedding API abuse | Per-request `http.Client{Timeout}` prevents hanging embedding calls |
| OTLP startup block | `context.WithTimeout(5s)` prevents boot failure when collector is absent |
| Loop abuse / prompt replay | Sliding-window circuit breaker trips after N identical hashes within window |
| Streaming exfiltration | `stream: true` rejected with `501 STREAMING_NOT_SUPPORTED` |
| SQL/NoSQL injection via Redis keys | Keys are built from `keyPrefixTenant + tenantID` — no user-controlled interpolation reaches the key path |

### OWASP alignment

| OWASP Top 10 | Status |
|---|---|
| A01 Broken Access Control | Bearer auth with O(1) tenant map; no token reuse across tenants |
| A02 Cryptographic Failures | No secrets stored; all keys in memory only; TLS enforced when API key present |
| A03 Injection | JSON marshal/unmarshal throughout; no format-string or concatenation-based JSON |
| A04 Insecure Design | Zero-trust tenant isolation at every layer |
| A05 Security Misconfiguration | `#nosec G304` annotated; `ReadHeaderTimeout` set; TLS auto-enabled |
| A06 Vulnerable Components | `go mod tidy` + dependabot recommended |
| A07 Auth Failures | 401 on missing/invalid Bearer; constant-time map lookup |
| A09 Security Logging | Structured JSON slog; no sensitive values in any log call |

---

## 13. V1 Constraints & V2 Roadmap

```mermaid
gantt
    title AgentMesh Roadmap
    dateFormat YYYY-MM
    axisFormat %b %Y

    section Phase 1 — Data Plane
    Multi-tenant reverse proxy     :done, 2026-01, 2026-02
    OTel instrumentation           :done, 2026-01, 2026-02
    Config validation              :done, 2026-01, 2026-02

    section Phase 2 — Guardrails & Budgets
    Loop detection circuit breaker :done, 2026-02, 2026-03
    Redis budget tracker           :done, 2026-02, 2026-03
    Budget middleware              :done, 2026-02, 2026-03

    section Phase 3 — Semantic Cache
    Qdrant vector store            :done, 2026-03, 2026-04
    OpenAI embedder                :done, 2026-03, 2026-04
    Cache middleware               :done, 2026-03, 2026-04

    section Phase 4 — V2 Features
    Streaming cache replay         :active, 2026-05, 2026-07
    Pre-authorised token reservation :2026-05, 2026-07
    Admin HTTP API                 :2026-06, 2026-08
    Local ONNX embedding runtime   :2026-07, 2026-09
    Multi-provider routing         :2026-08, 2026-10
```

| V1 Constraint | Root cause | V2 approach |
|---|---|---|
| `stream: true` → 501 | `ResponseRecorder` must buffer the full body to inspect tokens; streaming bypasses this | Chunked-transfer cache replay; token counting via SSE event parser |
| One-request budget overrun | Token cost not known until upstream responds | Pre-authorised reservation: `INCRBY estimatedTokens` pre-flight, adjust post-flight |
| `text-embedding-3-small` hardcoded | Single embedder per deployment | Config-driven model selection; local ONNX runtime for air-gapped deployments |
| No admin API | AdminPort reserved but unimplemented | Separate `http.Server` on AdminPort; tenant CRUD, budget reset, cache eviction endpoints |
| Single binary | Sufficient for v1 | Optional: separate control plane + data plane for horizontal scaling |
