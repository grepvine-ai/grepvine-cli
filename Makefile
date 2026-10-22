SHELL := /bin/sh

GOLANGCI_LINT := go tool -modfile=tools/golangci-lint/go.mod golangci-lint
VERSION ?= dev
LDFLAGS ?= -X main.version=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help build generate lint test test-race verify verify-generated

help: ## Print available targets and their descriptions
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

generate: ## Regenerate the API client from spec/openapi.json
	go tool ogen --target internal/httpapi --package httpapi --config ogen.yml --clean spec/openapi.json

lint: ## Run golangci-lint
	$(GOLANGCI_LINT) run

test: ## Run the tests
	go test ./...

test-race: ## Run the tests with the race detector
	go test -race ./...

build: ## Build gv into dist/
	go build -trimpath -ldflags '$(LDFLAGS)' -o dist/gv ./cmd/gv

verify: lint test build ## Run lint, tests and build

verify-generated: generate ## Fail when generated code is not current
	git diff --exit-code -- internal/httpapi
	test -z "$$(git status --porcelain -- internal/httpapi)"
