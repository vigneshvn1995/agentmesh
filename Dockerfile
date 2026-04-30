# syntax=docker/dockerfile:1
# ─────────────────────────────────────────────────────────────────────────────
# Stage 1 — Build
#
# Use --platform=$BUILDPLATFORM so the compiler always runs on the host
# architecture (fast). TARGETOS and TARGETARCH are injected by BuildKit when
# building a multi-arch image with `docker buildx build --platform ...`.
# ─────────────────────────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

# Install CA certificates so the binary can make TLS calls to upstream LLMs,
# Qdrant, and the OTLP collector without needing to mount a cert bundle.
RUN apk add --no-cache ca-certificates

WORKDIR /build

# Cache dependency downloads as a separate layer so they are not re-fetched
# on every source change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build arguments injected by docker/build-push-action for multi-arch builds.
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# CGO_ENABLED=0 produces a fully static binary that runs in a scratch image.
# -ldflags "-s -w" strips the symbol table and debug info to minimise image size.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o /out/agentmesh ./cmd/agentmesh/

# ─────────────────────────────────────────────────────────────────────────────
# Stage 2 — Runtime
#
# distroless/static-debian12:nonroot is ~2 MB, contains no shell, package
# manager, or OS utilities, and runs as UID 65532 (nonroot) by default —
# matching our Helm chart's runAsNonRoot: true security context.
# ─────────────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

# Copy the CA bundle from the builder so upstream TLS connections work.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=builder /out/agentmesh /agentmesh

# Proxy port and admin/health port.
EXPOSE 8080 9090

# The binary reads its config from -config flag; operators mount a ConfigMap
# at /etc/agentmesh/agentmesh.yaml via the Helm chart.
ENTRYPOINT ["/agentmesh"]
CMD ["-config", "/etc/agentmesh/agentmesh.yaml"]
