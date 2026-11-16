.DEFAULT_GOAL := help

.PHONY: help generate manifests build-cli test test-go test-agent lint lint-go lint-agent

help: ## List the available targets.
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "%-14s %s\n", $$1, $$2}'

CRD_DIR := deploy/pagerless-server/crds
ENVTEST_K8S_VERSION := 1.37.0

generate: ## Generate the deepcopy methods for the API types.
	go tool controller-gen object paths=./api/...

manifests: ## Generate the CustomResourceDefinitions.
	go tool controller-gen crd paths=./api/... output:crd:dir=$(CRD_DIR)

build-cli: ## Build the kubectl-triage plugin into bin/.
	go build -o bin/kubectl-triage ./cmd/kubectl-triage

test: test-go test-agent ## Run the Go and Python test suites.

test-go: ## Run the Go test suite, including the envtest suites.
	KUBEBUILDER_ASSETS="$$(go tool setup-envtest use -p path $(ENVTEST_K8S_VERSION))" go test ./...

test-agent: ## Run the Python test suite.
	cd agent && uv run --locked pytest

lint: lint-go lint-agent ## Run the Go and Python linters and type checker.

lint-go: ## Run go vet, golangci-lint, and the golangci-lint formatter check.
	go vet ./...
	go tool golangci-lint run
	go tool golangci-lint fmt --diff

lint-agent: ## Run ruff (lint and format check) and mypy.
	cd agent && uv run --locked ruff check .
	cd agent && uv run --locked ruff format --check .
	cd agent && uv run --locked mypy
