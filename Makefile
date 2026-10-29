.PHONY: help build test lint fmt vuln check

help: ## Show targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

build: ## Build the server binary to bin/App
	CGO_ENABLED=0 go build -trimpath -o bin/App ./cmd/App

test: ## Run all tests (integration tests need Docker for testcontainers)
	go test -race ./...

lint: ## golangci-lint (includes gofumpt/goimports checks)
	golangci-lint run

fmt: ## Apply gofumpt + goimports
	golangci-lint fmt

vuln: ## govulncheck against the Go vulnerability database
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

check: lint test vuln ## Everything CI runs
