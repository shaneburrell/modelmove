BINARY      := modelmove
PKG         := github.com/shaneburrell/modelmove
CMD         := ./cmd/modelmove
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
	-X $(PKG)/internal/cli.version=$(VERSION) \
	-X $(PKG)/internal/cli.commit=$(COMMIT) \
	-X $(PKG)/internal/cli.date=$(DATE)
COVERAGE_MIN := 80

.DEFAULT_GOAL := build

.PHONY: build
build: ## Build the binary into bin/
	@mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(CMD)

.PHONY: install
install: ## Install the binary into GOBIN
	go install -trimpath -ldflags '$(LDFLAGS)' $(CMD)

.PHONY: test
test: ## Run the test suite
	go test ./...

.PHONY: race
race: ## Run the test suite under the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run tests and fail below the coverage floor
	go test -coverprofile=coverage.out -covermode=atomic ./...
	@go tool cover -func=coverage.out | tail -1
	@total=$$(go tool cover -func=coverage.out | tail -1 | awk '{print substr($$3, 1, length($$3)-1)}'); \
	awk -v t="$$total" -v m="$(COVERAGE_MIN)" 'BEGIN { if (t+0 < m+0) { printf "coverage %s%% is below the %s%% floor\n", t, m; exit 1 } }'

.PHONY: cover-html
cover-html: cover ## Open the coverage report in a browser
	go tool cover -html=coverage.out

.PHONY: bench
bench: ## Run benchmarks
	go test -run '^$$' -bench . -benchmem ./...

.PHONY: fuzz
fuzz: ## Run each fuzz target briefly
	go test -run '^$$' -fuzz FuzzChunker -fuzztime 30s ./internal/chunk
	go test -run '^$$' -fuzz FuzzDecodeBinary -fuzztime 30s ./internal/manifest

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format the tree
	gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## Fail if the tree is not formatted
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then echo "these files need gofmt:"; echo "$$out"; exit 1; fi

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

.PHONY: e2e
e2e: build ## Local + fake-SSH smoke
	./scripts/e2e.sh

.PHONY: e2e-live
e2e-live: build ## Live sshd smoke (skips if ubuntu@127.0.0.1 is unreachable)
	./scripts/e2e-live.sh

.PHONY: check
check: fmt-check vet lint test ## Everything CI runs

.PHONY: snapshot
snapshot: ## Build release artifacts locally
	goreleaser release --snapshot --clean

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist coverage.out

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
