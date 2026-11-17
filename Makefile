.DEFAULT_GOAL := help

include hack/versions.env
PAGERLESS_BUILD_REVISION ?= local

.PHONY: help generate manifests build-cli build-images test test-go test-agent lint lint-go lint-agent up dev e2e down break reset cloud-up cloud-down cloud-check

help: ## List the available targets.
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "%-14s %s\n", $$1, $$2}'

CRD_DIR := deploy/pagerless-server/crds
ENVTEST_K8S_VERSION := 1.37.0

generate: ## Generate the deepcopy methods for the API types.
	go tool controller-gen object paths=./api/...

manifests: ## Generate the CustomResourceDefinitions.
	go tool controller-gen crd paths=./api/... output:crd:dir=$(CRD_DIR)

build-cli: ## Build the kubectl-triage plugin into bin/.
	go build -o bin/kubectl-triage ./cmd/kubectl-triage

build-images: ## Build the server, Executor, and agent container images locally.
	docker build --file Dockerfile.pagerless-server --build-arg "VCS_REF=$(PAGERLESS_BUILD_REVISION)" --tag $(PAGERLESS_SERVER_IMAGE) .
	docker build --file Dockerfile.pagerless-executor --build-arg "VCS_REF=$(PAGERLESS_BUILD_REVISION)" --tag $(PAGERLESS_EXECUTOR_IMAGE) .
	docker build --file Dockerfile.pagerless-agent --build-arg "VCS_REF=$(PAGERLESS_BUILD_REVISION)" --tag $(PAGERLESS_AGENT_IMAGE) .

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

dev: ## Build local images, load them into kind, and create the development stack.
	bash hack/dev.sh

e2e: ## Run the credential-free synthetic Replay end-to-end scenario in kind.
	bash evals/e2e/run.sh

down: ## Delete the kind cluster and the local Helm repository entry.
	bash hack/down.sh

break: ## Inject a Scenario into the Sandbox, for example make break SCENARIO=oom.
	bash hack/break.sh "$(SCENARIO)"

reset: ## Force-reset the Sandbox to the known-good tag.
	bash hack/reset.sh

cloud-up: ## Create the EKS session, deploy the stack, and wait for it to be healthy.
	bash hack/cloud-up.sh

cloud-down: ## Destroy the EKS session and prove that nothing billable remains.
	bash hack/cloud-down.sh

cloud-check: ## Check formatting, validation and lint of every Terraform configuration.
	terraform -chdir=infra fmt -check -recursive
	cd infra/bootstrap && terraform init -backend=false -input=false && terraform validate
	cd infra/ci && terraform init -backend=false -input=false && terraform validate
	cd infra && terraform init -backend=false -input=false && terraform validate
	cd infra && tflint --init && tflint --recursive
	bash -n hack/cloud-up.sh hack/cloud-down.sh hack/cloud-teardown-check.sh
	bash hack/cloud-down-order-check.sh
	bash hack/cloud-down-order-check.sh
	bash hack/render-cloud-manifest.sh deploy/cloud/karpenter/ec2nodeclass.yaml pagerless pagerless-karpenter-node >/dev/null
	bash hack/render-cloud-manifest.sh deploy/cloud/karpenter/nodepool.yaml pagerless pagerless-karpenter-node >/dev/null
	grep -q '__CLUSTER_NAME__' deploy/cloud/karpenter/nodepool.yaml
	grep -q '__NODE_ROLE_NAME__' deploy/cloud/karpenter/ec2nodeclass.yaml
