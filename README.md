[![CI](https://github.com/vigneshvn1995/agentmesh/actions/workflows/ci.yml/badge.svg)](https://github.com/vigneshvn1995/agentmesh/actions/workflows/ci.yml)
[![Go 1.26](https://img.shields.io/badge/go-1.26-blue.svg)](https://golang.org/dl/)
[![License: AGPL-3.0](https://img.shields.io/badge/license-AGPL--3.0-orange.svg)](LICENSE)

> **From dumb pipe to smart fabric.**  
> AgentMesh is a production-grade, multi-tenant LLM gateway written in Go 1.26. It sits transparently in front of any OpenAI-compatible API and turns every forwarded request into an opportunity to save compute, money, and carbon.

```
Agent / Caller
      │  Authorization: Bearer <inbound-key>
      ▼
┌──────────────────────────────────────────────────────────────────┐
│                           AgentMesh                              │
│                                                                  │
│  Auth → Guardrail → Semantic Cache → Budget → Reverse Proxy      │
│                           │                                      │
│                    X-AgentMesh-Cache: HIT                        │
│                    (zero tokens, zero upstream call)             │
└──────────────────────────────────────────────────────────────────┘
      │  Authorization: Bearer <upstream-key>          ▲
      ▼                                                │ cache hit
  OpenAI / Azure OpenAI / vLLM / …              Qdrant + Embedder
```

---

## Why AgentMesh?

Autonomous AI agents repeat themselves. A ReAct loop fires the same _"summarize this document"_ prompt dozens of times per hour. Without interception, every duplicate incurs:

- **GPU inference time** at the upstream provider — carbon and dollars burned for nothing
- **Token spend** deducted from your daily budget
- **Latency** — typically 800 ms–3 s for a GPT-4-class completion

AgentMesh intercepts semantically identical prompts before they leave your infrastructure, replying from a local Qdrant vector cache in **< 5 ms**. A cache hit costs **zero tokens**, triggers **zero upstream inference**, and records **zero budget deduction** in Redis.

Beyond caching, AgentMesh also handles:

- **Credential isolation** — upstream API keys never reach agent processes
- **Per-tenant and per-agent daily USD budgets** — backed by Redis with atomic INCRBY + ExpireNX
- **Loop detection** — sliding-window circuit breaker that trips when an agent floods the same prompt
- **Distributed tracing** — every request gets a W3C TraceContext span exported via OTLP gRPC

---

## Table of Contents

- [Feature Set](#feature-set)
- [How It Works](#how-it-works)
- [Quick Start](#quick-start)
- [Docker](#docker)
- [Kubernetes / Helm](#kubernetes--helm)
- [Drop-in Integration](#drop-in-integration)
- [Response Headers](#response-headers)
- [Configuration Reference](#configuration-reference)
- [Environment Variables](#environment-variables)
- [Observability](#observability)
- [Development](#development)
- [Project Structure](#project-structure)
- [V1 Limitations and Roadmap](#v1-limitations-and-roadmap)
- [Contributing](#contributing)
- [Security](#security)
- [License](#license)

---

## Feature Set

| Feature | Detail |
|---|---|
| **Multi-tenant auth** | Per-tenant inbound Bearer tokens mapped to real upstream credentials; O(1) lookup |
| **Credential isolation** | Upstream API keys are stored in a write-once map and never forwarded to callers; `[REDACTED]` in all log and trace output |
| **Loop detection** | Sliding-window circuit breaker on normalised prompt SHA-256 hashes; trips with `429 LOOP_DETECTED` |
| **Prompt normalisation** | UUIDs, ISO 8601 timestamps, punctuation, and filler prefixes stripped so semantically identical prompts produce the same hash regardless of incidental variation |
| **Semantic cache** | Cosine-similarity search via Qdrant; configurable threshold; per-tenant isolation enforced at the vector-filter level |
| **Token budgets** | Per-tenant and per-agent daily USD limits backed by Redis INCRBY + ExpireNX (MULTI/EXEC); fail-open or fail-closed |
| **OpenTelemetry** | W3C TraceContext propagation; OTLP gRPC export; every request gets a span via `otelhttp`; budget and cache decisions emit span events |
| **Structured logging** | JSON `log/slog` throughout; zero sensitive values in log output |
| **Health endpoint** | `GET /health` on the admin port returns `{"status":"ok"}` for Kubernetes liveness/readiness probes |
| **Graceful shutdown** | 15-second drain on `SIGINT`/`SIGTERM` |
| **Hermetic tests** | All tests run in-process using `miniredis` and mock interfaces — no external Redis, Qdrant, or LLM required |

---

## How It Works

### Request Lifecycle

Every inbound request passes through this pipeline in order:

```
otelhttp span
  └─ AuthMiddleware
       └─ GuardrailMiddleware
            ├─ 1 MiB body-size limit
            ├─ Streaming block (stream:true → 501)
            ├─ Prompt normalisation + SHA-256 hash
            └─ Sliding-window loop detection
                 └─ CacheMiddleware
                      ├─ Embed prompt → Qdrant search
                      ├─ HIT  → write cached response, return (budget never reached)
                      └─ MISS → continue ↓
                           └─ BudgetMiddleware
                                ├─ Pre-flight: check tenant + agent counters in Redis
                                ├─ Reject with 402 if either exceeds limit
                                └─ Pass through ↓
                                     └─ HandleProxy (ReverseProxy)
                                          └─ Post-flight: record token usage async
```

### Credential Flow

```
Client request                  AgentMesh process                  Upstream LLM
──────────────                  ─────────────────                  ────────────
Bearer am_live_abc123  ──────►  TenantMap lookup (O(1))
                                tenantID = "acme-corp"
                                upstreamKey = "sk-real-xyz"  ──►  Bearer sk-real-xyz
                                                                   (original key
                                                                    never forwarded)
```

All upstream keys are stored in an immutable map built once at startup. The `Config` struct is redacted immediately so keys cannot leak through logs or tracing.

### Budget Accounting

```
Request arrives
  │
  ├─ GET budget:tenant:<id>  ──►  ≥ limit?  YES → 402 BUDGET_EXCEEDED
  ├─ GET budget:agent:<id>   ──►  ≥ limit?  YES → 402 BUDGET_EXCEEDED
  │
  └─ (proxy to upstream)
       │
       └─ Read total_tokens from response
            │
            └─ MULTI
                 INCRBY budget:tenant:<id>  tokens
                 EXPIRENV budget:tenant:<id>  48h      (no-op if TTL already set)
                 INCRBY budget:agent:<id>  tokens
                 EXPIRENV budget:agent:<id>  48h
               EXEC
```

The 48-hour ExpireNX window resets naturally without a cron job. Concurrent requests cannot reset the window because `EXPIRE ... NX` is a no-op when a TTL is already set.

---

## Quick Start

### Prerequisites

| Requirement | Version | Notes |
|---|---|---|
| Go | 1.26+ | |
| Redis | 7.0+ | Required for budget enforcement |
| Qdrant | 1.9+ | Optional — only for semantic cache |

### Install from source

```bash
git clone https://github.com/vigneshvn1995/agentmesh
cd agentmesh
make build          # produces bin/agentmesh
```

### Minimal configuration

```yaml
# agentmesh.yaml
version: v1

server:
  proxy_port: 8080   # agents send requests here
  admin_port: 9090   # health probe endpoint

tenants:
  - tenant_id: acme-corp
    api_key: am_live_acme_abc123        # inbound key — agents send this
    upstream_url: https://api.openai.com/v1
    upstream_api_key: sk-real-key-xyz   # real credential — never exposed to agents

guardrails:
  enabled: true
  loop_detection:
    window_size: 5m        # sliding window for identical-prompt counting
    max_identical_hash: 3  # trips with 429 after 3 identical prompts in window

budget:
  per_agent_daily_usd: 2.00
  per_tenant_daily_usd: 50.00
  request_timeout: 30s
  tokens_per_usd: 1000      # adjust to match your model's pricing

redis:
  address: "localhost:6379"
  failure_mode: fail-open   # or "fail-closed" for strict enforcement
```

### Run

```bash
./bin/agentmesh -config agentmesh.yaml
```

Logs are emitted as JSON to stdout. Verify the proxy is up:

```bash
curl http://localhost:9090/health
# {"status":"ok"}
```

### Enable the semantic cache (optional)

Add to `agentmesh.yaml`:

```yaml
cache:
  enabled: true
```

Export credentials before starting:

```bash
export QDRANT_ENDPOINT="localhost"       # Qdrant host (gRPC port 6334 is default)
export QDRANT_API_KEY=""                 # empty for local unauthenticated instances
export OPENAI_API_KEY="sk-..."           # for text-embedding-3-small calls
```

---

## Docker

### Build the image locally

```bash
make docker-build
# Equivalent to: docker build -t ghcr.io/vigneshvn1995/agentmesh:latest .
```

The Dockerfile uses a two-stage build:
- **Stage 1**: `golang:1.26-alpine` compiles a fully static binary
- **Stage 2**: `distroless/static-debian12:nonroot` — ~2 MB runtime with no shell, no package manager, runs as UID 65532

### Run with Docker

```bash
docker run --rm \
  -p 8080:8080 \
  -p 9090:9090 \
  -v "$(pwd)/agentmesh.yaml:/etc/agentmesh/agentmesh.yaml:ro" \
  -e OTEL_EXPORTER_OTLP_INSECURE=true \
  ghcr.io/vigneshvn1995/agentmesh:latest
```

### Docker Compose (local stack)

```yaml
# docker-compose.yml
services:
  agentmesh:
    image: ghcr.io/vigneshvn1995/agentmesh:latest
    ports:
      - "8080:8080"
      - "9090:9090"
    volumes:
      - ./agentmesh.yaml:/etc/agentmesh/agentmesh.yaml:ro
    environment:
      OTEL_EXPORTER_OTLP_INSECURE: "true"
    depends_on:
      - redis

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
```

---

## Kubernetes / Helm

The production-ready Helm chart lives in `deploy/charts/agentmesh/`.

### Quick install (sandbox)

```bash
helm dependency update deploy/charts/agentmesh
helm install agentmesh deploy/charts/agentmesh \
  --set redis.enabled=true
```

### Production install

```bash
# 1. Create a Secret for your upstream API keys
kubectl create secret generic agentmesh-secrets \
  --from-literal=UPSTREAM_API_KEY_ACME=sk-real-key-xyz

# 2. Reference the Secret in values
cat > my-values.yaml <<EOF
image:
  repository: ghcr.io/vigneshvn1995/agentmesh
  tag: v1.0.0

extraEnv:
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: "otel-collector:4317"

# Point to an external Redis (managed service)
agentmeshConfig:
  redis:
    address: "my-redis.internal:6379"
    failure_mode: fail-closed
  tenants:
    - tenant_id: acme-corp
      api_key: am_live_abc123
      upstream_url: https://api.openai.com/v1
      upstream_api_key: "\$(UPSTREAM_API_KEY_ACME)"
EOF

helm install agentmesh deploy/charts/agentmesh -f my-values.yaml
```

### Helm chart highlights

| Feature | Detail |
|---|---|
| `autoscaling.enabled: true` | HPA targeting CPU 80%, min 2, max 10 replicas |
| `checksum/config` annotation | ConfigMap changes auto-trigger rolling restarts |
| Security contexts | `readOnlyRootFilesystem: true`, `runAsNonRoot: true`, no privilege escalation |
| Pod anti-affinity | `preferredDuringScheduling` spreads replicas across nodes |
| Subcharts | Bitnami Redis + Qdrant included as optional dependencies |

See [deploy/charts/agentmesh/README.md](deploy/charts/agentmesh/README.md) for the full values reference.

---

## Drop-in Integration

Replace your OpenAI base URL with the AgentMesh proxy address. No other code changes are required.

### Python — openai SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",  # ← point to AgentMesh
    api_key="am_live_acme_abc123",        # ← your AgentMesh inbound key
)

response = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "Explain quantum entanglement"}],
    extra_headers={"X-Agent-ID": "my-agent-001"},  # enables per-agent budgets
)
print(response.choices[0].message.content)
```

### Python — LangChain

```python
from langchain_openai import ChatOpenAI

llm = ChatOpenAI(
    openai_api_base="http://localhost:8080/v1",
    openai_api_key="am_live_acme_abc123",
    model_name="gpt-4o",
    default_headers={"X-Agent-ID": "langchain-agent"},
)
```

### cURL

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer am_live_acme_abc123" \
  -H "X-Agent-ID: my-agent-001" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

The optional `X-Agent-ID` header enables per-agent budget tracking. When omitted, the remote IP address is used as the agent identifier.

---

## Response Headers

| Header | Value | Meaning |
|---|---|---|
| `X-AgentMesh-Cache` | `HIT` | Response served from Qdrant; upstream LLM not called; zero tokens deducted |
| `X-AgentMesh-Cache` | `MISS` | Request forwarded to upstream; response stored in Qdrant asynchronously |

---

## Configuration Reference

### `server`

| Field | Type | Required | Description |
|---|---|---|---|
| `proxy_port` | int | ✓ | Port the LLM proxy listens on |
| `admin_port` | int | ✓ | Port for `GET /health` and future admin API |

### `tenants[]`

| Field | Type | Required | Description |
|---|---|---|---|
| `tenant_id` | string | ✓ | Unique identifier used in Redis keys and log output |
| `api_key` | string | ✓ | Inbound Bearer token agents present to AgentMesh |
| `upstream_url` | string | ✓ | Base URL of the upstream LLM API (must be a valid URL) |
| `upstream_api_key` | string | ✓ | Real credential forwarded to the upstream; redacted everywhere else |

### `guardrails`

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Master switch for all guardrail checks |
| `loop_detection.window_size` | duration | `5m` | Sliding window for identical-prompt counting |
| `loop_detection.max_identical_hash` | int | `3` | Maximum allowed identical prompts per window before 429 |

### `budget`

| Field | Type | Required | Description |
|---|---|---|---|
| `per_agent_daily_usd` | float | ✓ | Daily USD budget per agent |
| `per_tenant_daily_usd` | float | ✓ | Daily USD budget per tenant |
| `request_timeout` | duration | `2s` | Hard timeout for upstream and embedding calls |
| `tokens_per_usd` | float | `1000` | Conversion rate used to translate USD budgets into token counts. Adjust to match your model's pricing. |

> **Tip:** For GPT-4o at ~$5/1M tokens, set `tokens_per_usd: 200000`. For GPT-4o-mini at ~$0.15/1M, set `tokens_per_usd: 6666666`.

### `redis`

| Field | Type | Default | Description |
|---|---|---|---|
| `address` | string | — | `host:port` (required) |
| `password` | string | `""` | Redis `AUTH` password |
| `db` | int | `0` | Redis database index |
| `pool_size` | int | runtime | Connection pool size (defaults to runtime CPU count × 10) |
| `failure_mode` | string | `fail-open` | `fail-open` allows requests when Redis is down; `fail-closed` blocks with 503 |

### `cache` (optional)

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Enable Qdrant-backed semantic cache |
| `ttl` | duration | `24h` | Retention window for cached entries (reserved for future TTL-based eviction) |
| `max_size` | int | — | Reserved for future collection size cap |

---

## Environment Variables

| Variable | When used | Description |
|---|---|---|
| `QDRANT_ENDPOINT` | `cache.enabled: true` | Qdrant host without port (default gRPC port 6334 is used) |
| `QDRANT_API_KEY` | `cache.enabled: true` | Qdrant Cloud API key; leave empty for local unauthenticated instances |
| `OPENAI_API_KEY` | `cache.enabled: true` | API key for `text-embedding-3-small` embedding calls |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Always | OTLP collector gRPC address (default: `localhost:4317`) |
| `OTEL_EXPORTER_OTLP_INSECURE` | Always | Set `true` to connect to a local collector without TLS |

---

## Observability

### OpenTelemetry Traces

Every inbound request produces an `otelhttp` span named `"proxy"`. The following attributes and events are added by the middleware layers:

| Middleware | Attribute / Event | Value |
|---|---|---|
| Guardrail | `tenant_id`, `agent_id` | string |
| Guardrail | event `loop_detected` | `prompt_hash` attribute |
| Budget | `tenant_id`, `agent_id` | string |
| Budget | event `budget_exceeded` | `scope` = `tenant` or `agent` |
| Cache | `tenant_id` | string |
| Cache | event `cache_hit` | `tenant_id` attribute |

Traces are exported via OTLP gRPC. Compatible with Jaeger, Tempo, Datadog, Honeycomb, and any OTLP-capable backend.

**Jaeger quick start:**
```bash
docker run -d -p 4317:4317 -p 16686:16686 \
  -e COLLECTOR_OTLP_ENABLED=true \
  jaegertracing/all-in-one:latest

export OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317
export OTEL_EXPORTER_OTLP_INSECURE=true
./bin/agentmesh -config agentmesh.yaml
# Open http://localhost:16686
```

### Structured Logs

All log lines are JSON with `log/slog`. Key fields:

| Field | Present on |
|---|---|
| `tenant_id` | Every middleware log line |
| `agent_id` | Guardrail, budget middleware |
| `prompt_hash` | Loop-detected events |
| `tokens_saved` | Cache HIT (debug level) |
| `error` | All error paths |

Set log level by filtering on the `level` field, or configure your log aggregator to index `tenant_id` for per-tenant dashboards.

---

## Development

### Make targets

```bash
make help          # full list with descriptions
make build         # compile → bin/agentmesh
make test          # go test -race ./...  (requires gcc on Windows)
make test-local    # go test ./...        (no gcc required)
make lint          # golangci-lint run
make tidy          # go fmt + go mod tidy
make docker-build  # build Docker image
make helm-lint     # helm dependency update + helm lint
```

### Running tests

All tests are hermetic — no external services required:

```bash
make test-local
```

- **Unit tests** (`internal/guardrail`, `internal/budget`, `internal/config`) use in-process mocks and `miniredis`
- **Integration tests** (`test/`) spin up `miniredis` and mock HTTP servers in-process
- **Cache tests** use stub `Embedder` and `VectorStore` implementations

### Adding a new tenant

1. Add an entry under `tenants:` in your `agentmesh.yaml`
2. Restart (or trigger a rolling restart via `helm upgrade`)

No code changes are required. The config loader rebuilds the tenant map on startup.

### Adding a new upstream provider

Any OpenAI-compatible API (Azure OpenAI, vLLM, Ollama, Together AI, Groq, etc.) works as an `upstream_url`. Set the URL to the provider's base endpoint:

```yaml
tenants:
  - tenant_id: azure-tenant
    api_key: am_azure_key
    upstream_url: https://my-deployment.openai.azure.com/openai/deployments/gpt-4o
    upstream_api_key: <azure-api-key>
```

---

## Project Structure

```
agentmesh/
├── api/v1/                    # Config structs, Duration type, RedisFailureMode constants
│   └── config.go
├── cmd/agentmesh/             # main() — wiring, startup, graceful shutdown
│   └── main.go
├── deploy/
│   └── charts/agentmesh/     # Production-ready Helm chart
│       ├── Chart.yaml
│       ├── values.yaml
│       ├── README.md
│       └── templates/
├── docs/
│   └── architecture.md        # Deep-dive: sequence diagrams, ADRs, Redis key design
├── internal/
│   ├── budget/                # Redis-backed token budget tracker + HTTP middleware
│   │   ├── tracker.go         # INCRBY + ExpireNX, fail-open/fail-closed
│   │   ├── tracker_test.go    # miniredis-backed unit tests
│   │   └── middleware.go      # Pre-flight check, post-flight async recording
│   ├── cache/                 # Semantic response cache
│   │   ├── ports.go           # CacheEntry, Embedder, VectorStore interfaces
│   │   ├── middleware.go      # Cache lookup, HIT replay, MISS async store
│   │   ├── qdrant.go          # Qdrant VectorStore implementation
│   │   ├── openai_embedder.go # OpenAI-compatible embeddings client
│   │   └── noop_embedder.go   # Stub for testing
│   ├── config/                # YAML loader, validation, tenant map, credential redaction
│   │   ├── loader.go
│   │   └── loader_test.go
│   ├── ctxkeys/               # Type-safe context keys (tenant propagation)
│   ├── guardrail/             # Prompt normaliser, SHA-256 hasher, circuit breaker
│   │   ├── normalizer.go      # UUID/timestamp stripping, filler prefix removal
│   │   ├── normalizer_test.go # 50+ table-driven cases
│   │   ├── circuitbreaker.go  # Sliding-window loop detection
│   │   └── circuitbreaker_test.go
│   ├── httputil/              # ResponseRecorder (pooled), WriteJSONError
│   ├── proxy/                 # Core reverse proxy server
│   │   ├── server.go          # NewServer, AuthMiddleware, HandleProxy, Start, StartAdmin
│   │   ├── middleware.go      # GuardrailMiddleware (body limit, streaming block, breaker)
│   │   └── server_test.go
│   ├── secwipe/               # Build-tag shim for runtime/secret (Go experiment)
│   └── telemetry/             # OTel TracerProvider initialisation (OTLP gRPC)
├── test/                      # End-to-end integration tests
│   ├── integration_test.go    # Full middleware chain tests
│   └── cache_integration_test.go
├── .github/workflows/
│   ├── ci.yml                 # Test + lint + helm lint on push/PR
│   └── release.yml            # Docker + Helm OCI publish on v* tags
├── .golangci.yml              # golangci-lint configuration
├── Dockerfile                 # Multi-stage: golang:1.26-alpine → distroless
├── Makefile                   # Self-documenting build targets
└── go.mod
```

---

## V1 Limitations and Roadmap

| Limitation | Planned resolution |
|---|---|
| `stream: true` blocked with `501` | V2: chunked-transfer cache replay via `http.Flusher` |
| Budget enforcement is eventually consistent by one request | V2: pre-authorised token reservation before upstream call |
| Single Qdrant collection per deployment (no per-tenant collections) | V2: configurable collection-per-tenant option |
| Embedding model hard-coded to `text-embedding-3-small` | V2: configurable model + local ONNX runtime option |
| No admin API | V2: admin HTTP server with tenant CRUD, budget reset, cache invalidation |
| Qdrant collection must be pre-created | V2: auto-create collection on startup |

---

## Contributing

Contributions are welcome! Please read the following before opening a PR:

1. **Open an issue first** for non-trivial features or design changes.
2. **Fork, branch, and PR** — branch names should be `feat/...`, `fix/...`, or `chore/...`.
3. **All tests must pass**: `make test-local`
4. **Lint must be clean**: `make lint`
5. **Keep PRs focused** — one logical change per PR makes review faster.
6. **Commit style**: imperative mood, present tense (`"add budget middleware"`, not `"added"`).

### Running the full CI suite locally

```bash
make all       # clean → tidy → lint → test → build
make helm-lint # validate the Helm chart
```

---

## Security

AgentMesh is designed with a zero-trust posture for credential handling:

- **Upstream API keys** are stored in a write-once `map[string]string` built at startup. The `Config` struct is redacted immediately — keys are replaced with `"[REDACTED]"` — so they cannot leak through structured logs or OpenTelemetry spans.
- **Inbound API keys** are never logged, traced, or forwarded upstream.
- **Prompt content** is never set as a span attribute or log field (only the normalised SHA-256 hash is recorded on loop-detected events).
- **Request bodies** are size-limited to 1 MiB before any parsing occurs.
- The container image runs as `nonroot` (UID 65532) with `readOnlyRootFilesystem: true`.

**Reporting vulnerabilities:** Please do **not** open a public GitHub issue for security vulnerabilities. Email `vigneshvn1995@gmail.com` with the details. We will respond within 48 hours and coordinate disclosure.

---

## License

[GNU Affero General Public License v3.0](LICENSE) — see [LICENSE](LICENSE) for the full text.

> **AGPL-3.0 summary:** You may use, modify, and distribute this software freely. If you run a modified version as a network service (e.g. a hosted SaaS product), you must make the complete corresponding source code available to users of that service under the same license.


```
Agent / Caller
      │  Bearer <inbound-key>
      ▼
┌─────────────────────────────────────────────────────────┐
│                       AgentMesh                         │
│  Auth → Guardrail → Semantic Cache → Budget → Proxy     │
└─────────────────────────────────────────────────────────┘
      │  Bearer <real-upstream-key>          ▲
      ▼                                      │ cache hit (no LLM call)
  OpenAI / Azure / vLLM / …            Qdrant + Embedder
```

---

