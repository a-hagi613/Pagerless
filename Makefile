.DEFAULT_GOAL := help

include hack/versions.env
PAGERLESS_BUILD_REVISION ?= local

.PHONY: help generate manifests build-cli build-images dashboard-build dashboard-check test test-go test-agent lint lint-go lint-agent validate verify-chart preflight dev down install dashboard-preview

help: ## List the available targets.
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "%-16s %s\n", $$1, $$2}'

CHART_CRD_DIR := charts/pagerless/crds
ENVTEST_K8S_VERSION := 1.37.0
GO_TEST_FLAGS ?= -p 1 -parallel 2

generate: ## Generate the deepcopy methods for the API types.
	go tool controller-gen object paths=./api/...

manifests: ## Generate the CustomResourceDefinitions into the chart.
	# Cost is an estimate in USD, so the status carries a bounded JSON number for it.
	go tool controller-gen crd:allowDangerousTypes=true paths=./api/... output:crd:dir=$(CHART_CRD_DIR)

CLI_VERSION ?= $(shell sed -n 's/^version: //p' charts/pagerless/Chart.yaml)
CLI_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
CLI_BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

build-cli: ## Build the kubectl-page plugin into bin/.
	go build -trimpath -ldflags "-X github.com/a-hagi613/pagerless/internal/cli.Version=$(CLI_VERSION) -X github.com/a-hagi613/pagerless/internal/cli.Commit=$(CLI_COMMIT) -X github.com/a-hagi613/pagerless/internal/cli.BuildDate=$(CLI_BUILD_DATE)" -o bin/kubectl-page ./cmd/kubectl-page

dashboard-build: ## Build the dashboard into dashboard/dist for the Go embed.
	cd dashboard && npm ci && npm run build

dashboard-check: ## Typecheck and lint the dashboard.
	cd dashboard && npm ci && npm run typecheck && npm run lint

build-images: ## Build the server and agent container images locally.
	docker build --file Dockerfile.pagerless-server --build-arg "VCS_REF=$(PAGERLESS_BUILD_REVISION)" --tag $(PAGERLESS_SERVER_IMAGE) .
	docker build --file Dockerfile.pagerless-agent --build-arg "VCS_REF=$(PAGERLESS_BUILD_REVISION)" --tag $(PAGERLESS_AGENT_IMAGE) .

test: test-go test-agent ## Run the Go and Python test suites.

test-go: ## Run the Go test suite, including the envtest and chart suites.
	KUBEBUILDER_ASSETS="$$(go tool setup-envtest use -p path $(ENVTEST_K8S_VERSION))" go test $(GO_TEST_FLAGS) ./...

test-agent: ## Run the Python test suite.
	cd agent && uv run --locked pytest -c pyproject.toml tests

lint: lint-go lint-agent ## Run the Go and Python linters and type checker.

lint-go: ## Run go vet, golangci-lint, and the golangci-lint formatter check.
	go vet ./...
	go tool golangci-lint run
	go tool golangci-lint fmt --diff

lint-agent: ## Run ruff (lint and format check) and mypy.
	cd agent && uv run --locked ruff check .
	cd agent && uv run --locked ruff format --check .
	cd agent && uv run --locked mypy src tests

validate: ## Render the chart and validate it with the pinned kubeconform.
	bash hack/validate-manifests.sh

verify-chart: ## Install the chart on a throwaway kind cluster and prove the read-only grant.
	bash hack/verify-chart.sh

preflight: ## Check the local prerequisites of make dev.
	bash hack/preflight.sh

dev: ## Build local images, load them into kind, and install the chart.
	bash hack/dev.sh

down: ## Delete the local development kind cluster.
	bash hack/down.sh

install: ## Install Pagerless into an existing Kubernetes cluster.
	bash hack/install.sh

dashboard-preview: ## Start the live Dashboard preview on port 8080.
	bash hack/dashboard-preview.sh start
