BINARY_NAME   := agentmesh
DOCKER_IMAGE  := ghcr.io/vigneshvn1995/agentmesh
VERSION       := latest
CHART_DIR     := deploy/charts/agentmesh

.DEFAULT_GOAL := help

.PHONY: all tidy build test test-local lint clean docker-build helm-deps helm-lint help

all: clean tidy lint test build ## Clean, tidy, lint, test, and build

tidy: ## Format code and tidy modfile
	go fmt ./...
	go mod tidy

build: ## Build the agentmesh binary
	go build -ldflags="-s -w" -o bin/$(BINARY_NAME) ./cmd/agentmesh/

test: ## Run unit and integration tests with the race detector
	go test -v -race ./...

test-local: ## Run tests without the race detector (Windows / no gcc)
	go test ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

clean: ## Remove build artifacts
	rm -rf bin/
	go clean

docker-build: ## Build the Docker image
	docker build -t $(DOCKER_IMAGE):$(VERSION) .

helm-deps: ## Update Helm chart dependencies
	helm dependency update $(CHART_DIR)

helm-lint: helm-deps ## Lint the Helm chart
	helm lint $(CHART_DIR)

help: ## Show this help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
