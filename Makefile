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

.PHONY: parity-up parity parity-selftest parity-smoke

parity-up: ## Start Swift + Go + Postgres for parity runs (PARITY_SWIFT_IMAGE overrides)
	scripts/parity-up.sh

parity: ## Compare Swift and Go on parity/scenarios (MODULE=<m> for one module)
	go run ./parity/cmd/parity compare --module "$(MODULE)" \
	  --swift http://localhost:18080 --swift-db postgres://lv:lv@localhost:15432/parity_swift \
	  --go http://localhost:18081 --go-db postgres://lv:lv@localhost:15432/parity_go

parity-selftest: ## Swift vs Swift: proves scenarios are deterministic (MODULE=<m>)
	go run ./parity/cmd/parity compare --module "$(MODULE)" \
	  --swift http://localhost:18080 --swift-db postgres://lv:lv@localhost:15432/parity_swift \
	  --go http://localhost:18082 --go-db postgres://lv:lv@localhost:15432/parity_swift2

parity-smoke: ## Gate G2 against api-go-staging (needs STAGING_JWT, MODULE)
	gh run download --name swift-transcripts --dir swift-transcripts
	KUBECONFIG=$(HOME)/.kube/maat.yaml kubectl -n horus port-forward deploy/api-go-staging 18090:8080 & \
	  pf=$$!; sleep 3; \
	  go run ./parity/cmd/parity smoke --target http://localhost:18090 --module "$(MODULE)"; rc=$$?; \
	  kill $$pf; exit $$rc

.PHONY: gen schema-parity

gen: ## Regenerate module models (oapi-codegen) and queries (sqlc)
	@for cfg in internal/modules/*/oapi-codegen.yaml; do go tool oapi-codegen -config $$cfg api/openapi.yaml || exit 1; done
	sqlc generate

schema-parity: ## goose builds exactly the Swift (Fluent) schema
	scripts/schema-parity.sh
