.DEFAULT_GOAL := help

include hack/versions.env

.PHONY: help generate manifests test test-go test-agent lint lint-go lint-agent up down break reset

help: ## List the available targets.
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "%-14s %s\n", $$1, $$2}'

CRD_DIR := deploy/pagerless-server/crds
ENVTEST_K8S_VERSION := 1.37.0

generate: ## Generate the deepcopy methods for the API types.
	go tool controller-gen object paths=./api/...

manifests: ## Generate the CustomResourceDefinitions.
	go tool controller-gen crd paths=./api/... output:crd:dir=$(CRD_DIR)

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

up: ## Create the kind cluster, install Argo CD, and deploy the Sandbox.
	bash hack/up.sh

down: ## Delete the kind cluster and the local Helm repository entry.
	bash hack/down.sh

break: ## Inject a Scenario into the Sandbox, for example make break SCENARIO=oom.
	bash hack/break.sh "$(SCENARIO)"

reset: ## Force-reset the Sandbox to the known-good tag.
	bash hack/reset.sh
