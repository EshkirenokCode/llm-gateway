BINARY  := gateway
PKG     := ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: help build run test test-docker lint fmt tidy docker clean

help: ## Show available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "%-10s %s\n", $$1, $$2}'

build: ## Compile the gateway binary into ./bin
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/gateway

run: ## Run the gateway from source
	go run ./cmd/gateway

test: ## Run all tests with the race detector
	go test -race -count=1 $(PKG)

test-docker: ## Run tests with -race inside Linux (no gcc needed on the host)
	docker run --rm -v "$(CURDIR)":/src -w /src golang:1.23-alpine sh -c "apk add --no-cache gcc musl-dev >/dev/null && go test -race -count=1 $(PKG)"

lint: ## Run golangci-lint
	golangci-lint run

fmt: ## Format the codebase
	go fmt $(PKG)

tidy: ## Sync go.mod/go.sum
	go mod tidy

docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t llm-gateway:$(VERSION) .

clean: ## Remove build output
	rm -rf bin coverage.out
