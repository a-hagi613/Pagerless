#!/usr/bin/env bash
# Creates one EKS session: Terraform, Argo CD, Karpenter, and the same root
# Application that kind runs (ADR-0009, ADR-0010).
#
# The session is the only billable state this repository can create. It ends
# with `make cloud-down`, which destroys the cluster and proves that nothing
# billable remains.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions
load_env

CLOUD_ACCOUNT_ID="${PAGERLESS_CLOUD_ACCOUNT_ID:-207567775100}"
CLOUD_REGION="${AWS_REGION:-us-east-1}"
CLOUD_CLUSTER="${PAGERLESS_CLOUD_CLUSTER:-pagerless}"
CLOUD_CONTEXT="${PAGERLESS_CLOUD_CONTEXT:-pagerless-eks}"

# Every helper that talks to the cluster targets the session, not the kind cluster.
export PAGERLESS_KUBE_CONTEXT="$CLOUD_CONTEXT"
export AWS_REGION="$CLOUD_REGION"
export MODEL_PROVIDER=bedrock

PLATFORM_REPO_URL="${PLATFORM_REPO_URL:-https://github.com/a-hagi613/pagerless}"
PLATFORM_REVISION="${PLATFORM_REVISION:-main}"

require_commands terraform aws kubectl helm uv

# The two fine-grained tokens of ADR-0015 reach .env as GITHUB_READ_TOKEN and
# GITHUB_WRITE_TOKEN; an operator who exported them as SANDBOX_READ_TOKEN and
# SANDBOX_WRITE_TOKEN gets the same result.
GITHUB_READ_TOKEN="${GITHUB_READ_TOKEN:-${SANDBOX_READ_TOKEN:-}}"
GITHUB_WRITE_TOKEN="${GITHUB_WRITE_TOKEN:-${SANDBOX_WRITE_TOKEN:-}}"
export GITHUB_READ_TOKEN GITHUB_WRITE_TOKEN
require_env SANDBOX_URL GITHUB_READ_TOKEN GITHUB_WRITE_TOKEN

[ "$(terraform version | head -n 1)" = "Terraform v${TERRAFORM_VERSION}" ] ||
	die "Terraform ${TERRAFORM_VERSION} is required; this is $(terraform version | head -n 1)."
helm_installed="$(helm version --short | cut -d '+' -f 1 | sed 's/^v//')"
[ "$helm_installed" = "${HELM_VERSION#v}" ] ||
	die "Helm ${HELM_VERSION} is required; this is $(helm version --short)."

account="$(aws sts get-caller-identity --query Account --output text)"
[ "$account" = "$CLOUD_ACCOUNT_ID" ] ||
	die "this session must run in account ${CLOUD_ACCOUNT_ID}, but the credentials are for ${account}."

echo "Creating the EKS session in account ${CLOUD_ACCOUNT_ID}, region ${CLOUD_REGION}."
echo "The served cluster is destroyed by make cloud-down, and a session must not span the nightly teardown at 03:17 UTC."

# 1. Infrastructure. The state bucket already exists (infra/bootstrap).
terraform -chdir="$REPO_ROOT/infra" init -input=false
terraform -chdir="$REPO_ROOT/infra" apply -input=false -auto-approve

# 2. A kubeconfig context that cannot be confused with the local kind cluster,
# then a wait for the system node group to register its first node.
aws eks update-kubeconfig --name "$CLOUD_CLUSTER" --region "$CLOUD_REGION" --alias "$CLOUD_CONTEXT" >/dev/null
ready_nodes=0
for _ in $(seq 1 60); do
	ready_nodes=$(kc get nodes --no-headers 2>/dev/null | awk '$2 == "Ready"' | wc -l)
	[ "$ready_nodes" -ge 1 ] && break
	sleep 10
done
[ "$ready_nodes" -ge 1 ] || die "no node joined the cluster within ten minutes."

# 3. Argo CD from the same pinned chart and values as kind.
helm repo add argo "$ARGOCD_CHART_REPO" --force-update >/dev/null
helm upgrade --install argocd argo/argo-cd \
	--version "$ARGOCD_CHART_VERSION" \
	--namespace argocd --create-namespace \
	--values "$REPO_ROOT/deploy/argocd/values.yaml" \
	--kube-context "$CLOUD_CONTEXT" \
	--wait --timeout 15m

# 4. Karpenter, from its signed OCI chart. Pod Identity supplies the controller's
# credentials, so the chart needs the cluster name and the interruption queue
# rather than a role annotation.
helm registry logout public.ecr.aws >/dev/null 2>&1 || true
helm upgrade --install karpenter oci://public.ecr.aws/karpenter/karpenter \
	--version "$KARPENTER_CHART_VERSION" \
	--namespace kube-system \
	--kube-context "$CLOUD_CONTEXT" \
	--set "settings.clusterName=$CLOUD_CLUSTER" \
	--set "settings.interruptionQueue=$(terraform -chdir="$REPO_ROOT/infra" output -raw karpenter_interruption_queue_name)" \
	--set controller.resources.requests.cpu=1 \
	--set controller.resources.requests.memory=1Gi \
	--set controller.resources.limits.cpu=1 \
	--set controller.resources.limits.memory=1Gi \
	--wait --timeout 10m

# The NodePool and EC2NodeClass name the cluster and the node role, which the
# Terraform outputs own.
karpenter_node_role="$(terraform -chdir="$REPO_ROOT/infra" output -raw karpenter_node_role_name)"
bash "$REPO_ROOT/hack/render-cloud-manifest.sh" \
	"$REPO_ROOT/deploy/cloud/karpenter/ec2nodeclass.yaml" "$CLOUD_CLUSTER" "$karpenter_node_role" | kc apply -f -
bash "$REPO_ROOT/hack/render-cloud-manifest.sh" \
	"$REPO_ROOT/deploy/cloud/karpenter/nodepool.yaml" "$CLOUD_CLUSTER" "$karpenter_node_role" | kc apply -f -
kc apply -f "$REPO_ROOT/deploy/cloud/storage"

# 5. The Alertmanager credential must exist before the observability Application
# creates Alertmanager, which is the same order `make up` uses.
uv run --no-project --python "$PYTHON_VERSION" "$REPO_ROOT/hack/provision-authorization.py" alerts

kc create namespace pagerless-system --dry-run=client -o yaml | kc apply -f -
kc create namespace pagerless --dry-run=client -o yaml | kc apply -f -

# 6. Secrets from .env. Tokens reach kubectl through file descriptors, not argv.
# The agent's inference Secret stays empty: EKS Pod Identity supplies the
# credentials of ADR-0023, and pagerless-agent-credentials has no keys at all.
kc -n pagerless create secret generic github-read \
	--from-file=token=<(printf '%s' "$GITHUB_READ_TOKEN") \
	--dry-run=client -o yaml | kc apply -f -
kc -n pagerless create secret generic github-write \
	--from-file=token=<(printf '%s' "$GITHUB_WRITE_TOKEN") \
	--dry-run=client -o yaml | kc apply -f -
kc -n pagerless create configmap pagerless-sandbox \
	--from-literal=url="$SANDBOX_URL" \
	--from-literal=githubRepository="${PAGERLESS_GITHUB_REPOSITORY:-}" \
	--dry-run=client -o yaml | kc apply -f -
kc -n argocd create secret generic sandbox-repository \
	--from-literal=type=git \
	--from-literal=url="$SANDBOX_URL" \
	--from-literal=username=pagerless \
	--from-file=password=<(printf '%s' "$GITHUB_READ_TOKEN") \
	--dry-run=client -o yaml |
	kc label --local -f - argocd.argoproj.io/secret-type=repository -o yaml |
	kc apply -f -

# 7. The same root Application as kind, over the same chart.
helm template pagerless-root "$REPO_ROOT/deploy/root" \
	--namespace argocd \
	--set "platform.repoURL=$PLATFORM_REPO_URL" \
	--set "platform.targetRevision=$PLATFORM_REVISION" \
	--set "sandbox.repoURL=$SANDBOX_URL" \
	--set-string "mcp.githubHost=${PAGERLESS_MCP_GITHUB_HOST:-}" |
	kc apply -f -

# 8. Wait for the stack, then provision the read-only MCP identities. Node
# provisioning through Karpenter and image pulls are slower than kind, so the
# waits are longer than `make up`'s.
wait_for_application root "" 600 synced
wait_for_application sandbox "" 900
for app in kube-prometheus-stack loki tempo opentelemetry-collector observability-alerts; do
	wait_for_application "$app" "" 1800
done
kc -n sandbox rollout status deployment/checkout --timeout=600s
uv run --no-project --python "$PYTHON_VERSION" "$REPO_ROOT/hack/provision-mcp.py"
wait_for_application mcp "" 600
if kc -n pagerless get deployment pagerless-server >/dev/null 2>&1; then
	# Environment variables from a ConfigMap are read only when a container starts.
	kc -n pagerless rollout restart deployment/pagerless-server
fi
wait_for_application pagerless-server "" 900
wait_for_application pagerless-executor "" 900
uv run --no-project --python "$PYTHON_VERSION" "$REPO_ROOT/hack/provision-authorization.py" approver

cat <<EOF

The EKS session is up in ${CLOUD_REGION}. Every Argo CD Application is Synced and Healthy.

The dashboard and Grafana are not exposed; reach them with a port-forward:
  kubectl --context ${CLOUD_CONTEXT} -n pagerless port-forward svc/pagerless-server 8080:8080
  kubectl --context ${CLOUD_CONTEXT} -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80

Break the Sandbox, and drive the CLI, against this cluster by exporting the context:
  export PAGERLESS_KUBE_CONTEXT=${CLOUD_CONTEXT}
  make break SCENARIO=oom

Destroy the session, and prove that nothing billable remains, with:
  make cloud-down
EOF
