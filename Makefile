.DEFAULT_GOAL := help

include hack/versions.env
PAGERLESS_BUILD_REVISION ?= local

.PHONY: help generate manifests build-cli build-images dashboard-build dashboard-check test test-go test-agent test-eval lint lint-go lint-agent validate up dev e2e eval eval-replay eval-table eval-table-check prompts-check down break reset cloud-up cloud-down cloud-check

help: ## List the available targets.
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "%-14s %s\n", $$1, $$2}'

CRD_DIR := deploy/pagerless-server/crds
ENVTEST_K8S_VERSION := 1.37.0

# Evaluation defaults. The paid evaluation runs each Scenario twice (ADR-0010);
# the credential-free CI subset runs three Scenarios once in Replay.
SCENARIOS ?= all
REPEATS ?= 2
MAX_TOKENS ?= 0
MAX_COST_USD ?= 0
REPLAY_SCENARIOS ?= oom,unschedulable,bad-config

generate: ## Generate the deepcopy methods for the API types.
	go tool controller-gen object paths=./api/...

manifests: ## Generate the CustomResourceDefinitions.
	# Cost is an estimate in USD, so the status carries a bounded JSON number for it.
	go tool controller-gen crd:allowDangerousTypes=true paths=./api/... output:crd:dir=$(CRD_DIR)

build-cli: ## Build the kubectl-triage plugin into bin/.
	go build -o bin/kubectl-triage ./cmd/kubectl-triage

dashboard-build: ## Build the dashboard into dashboard/dist for the Go embed.
	cd dashboard && npm ci && npm run build

dashboard-check: ## Typecheck and lint the dashboard.
	cd dashboard && npm ci && npm run typecheck && npm run lint

build-images: ## Build the server, Executor, and agent container images locally.
	docker build --file Dockerfile.pagerless-server --build-arg "VCS_REF=$(PAGERLESS_BUILD_REVISION)" --tag $(PAGERLESS_SERVER_IMAGE) .
	docker build --file Dockerfile.pagerless-executor --build-arg "VCS_REF=$(PAGERLESS_BUILD_REVISION)" --tag $(PAGERLESS_EXECUTOR_IMAGE) .
	docker build --file Dockerfile.pagerless-agent --build-arg "VCS_REF=$(PAGERLESS_BUILD_REVISION)" --tag $(PAGERLESS_AGENT_IMAGE) .

test: test-go test-agent test-eval ## Run the Go, Python, and evaluation-harness test suites.

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

validate: ## Render every manifest and validate it with the pinned kubeconform.
	bash hack/validate-manifests.sh

up: ## Create the kind cluster, install Argo CD, and deploy the Sandbox.
	bash hack/up.sh

dev: ## Build local images, load them into kind, and create the development stack.
	bash hack/dev.sh

e2e: ## Run the credential-free synthetic Replay end-to-end scenario in kind.
	bash evals/e2e/run.sh

eval: ## Run the evaluation on the local stack, score every run, and record the results.
	bash evals/harness/eval.sh --scenarios "$(SCENARIOS)" --repeats "$(REPEATS)" --max-tokens "$(MAX_TOKENS)" --max-cost-usd "$(MAX_COST_USD)"

eval-replay: ## Run the three-Scenario credential-free Replay subset in the isolated adapter kind cluster.
	PAGERLESS_E2E_EVAL=1 PAGERLESS_E2E_SCENARIOS="$(REPLAY_SCENARIOS)" PAGERLESS_E2E_REPEATS=1 PAGERLESS_E2E_REQUIRE_PASS=1 bash evals/e2e/run.sh

eval-table: ## Regenerate the README results table from evals/results.
	cd evals/harness && uv run --no-project --python $(PYTHON_VERSION) python -m pagerless_eval.table --update-readme

eval-table-check: ## Fail when README.md's results table is out of date.
	cd evals/harness && uv run --no-project --python $(PYTHON_VERSION) python -m pagerless_eval.table --check

test-eval: ## Run the evaluation harness's unit tests (no cluster required).
	cd evals/harness && uv run --no-project --python $(PYTHON_VERSION) python -m unittest discover -s tests -t .

prompts-check: ## Run the prompt-injection suite against the agent's prompts without a paid call.
	cd agent && uv sync --locked
# promptfoo declares optional peer dependencies, among them gcp-metadata for the
# mongodb driver, and npm's strict `ci` lock validation rejects them while its
# installer resolves them. The committed lock still pins every package that this
# dev-only suite runs, and peer resolution plays no part in it.
	cd evals/prompts && npm ci --no-audit --no-fund --legacy-peer-deps && PATH="$(CURDIR)/agent/.venv/bin:$$PATH" ./node_modules/.bin/promptfoo eval -c promptfooconfig.offline.yaml --no-cache

down: ## Delete the kind cluster and the local Helm repository entry.
	bash hack/down.sh

break: ## Inject a Scenario, for example make break SCENARIO=oom.
	bash hack/break.sh "$(SCENARIO)"

reset: ## Force-reset the Sandbox to the known-good tag and remove chaos experiments.
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
	bash -n hack/cloud-up.sh hack/cloud-down.sh hack/cloud-teardown-check.sh hack/cloud-volume-sweep.sh \
		hack/cloud-teardown-check-test.sh hack/cloud-volume-sweep-test.sh hack/bootstrap-stack.sh hack/up.sh
	bash hack/cloud-down-order-check.sh
	bash hack/cloud-teardown-check-test.sh
	bash hack/cloud-volume-sweep-test.sh
	bash hack/render-cloud-manifest.sh deploy/cloud/karpenter/ec2nodeclass.yaml pagerless pagerless-karpenter-node >/dev/null
	bash hack/render-cloud-manifest.sh deploy/cloud/karpenter/nodepool.yaml pagerless pagerless-karpenter-node >/dev/null
	grep -q '__CLUSTER_NAME__' deploy/cloud/karpenter/nodepool.yaml
	grep -q '__NODE_ROLE_NAME__' deploy/cloud/karpenter/ec2nodeclass.yaml
