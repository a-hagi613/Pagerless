#!/usr/bin/env bash
# Creates one EKS session: Terraform, Karpenter, and then the same bootstrap that
# `make up` runs on kind (hack/bootstrap-stack.sh), so the two paths cannot drift.
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
echo "The cluster is destroyed by make cloud-down, and a session must not span the nightly teardown at 03:17 UTC."

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

# 3. Karpenter, from its signed OCI chart. Pod Identity supplies the controller's
# credentials, so the chart needs the cluster name and the interruption queue
# rather than a role annotation.
helm registry logout public.ecr.aws >/dev/null 2>&1 || true
# One controller: the chart defaults to two replicas that each request a CPU, and
# the single t3.large system node cannot schedule both, so the install never
# becomes ready. A session runs one controller.
helm upgrade --install karpenter oci://public.ecr.aws/karpenter/karpenter \
	--version "$KARPENTER_CHART_VERSION" \
	--namespace kube-system \
	--kube-context "$CLOUD_CONTEXT" \
	--set "settings.clusterName=$CLOUD_CLUSTER" \
	--set "settings.interruptionQueue=$(terraform -chdir="$REPO_ROOT/infra" output -raw karpenter_interruption_queue_name)" \
	--set replicas=1 \
	--set controller.resources.requests.cpu=1 \
	--set controller.resources.requests.memory=1Gi \
	--set controller.resources.limits.cpu=1 \
	--set controller.resources.limits.memory=1Gi \
	--wait --timeout 10m

# The NodePool and EC2NodeClass name the cluster and the node role, which the
# Terraform outputs own. The StorageClass must exist before any claim does.
karpenter_node_role="$(terraform -chdir="$REPO_ROOT/infra" output -raw karpenter_node_role_name)"
bash "$REPO_ROOT/hack/render-cloud-manifest.sh" \
	"$REPO_ROOT/deploy/cloud/karpenter/ec2nodeclass.yaml" "$CLOUD_CLUSTER" "$karpenter_node_role" | kc apply -f -
bash "$REPO_ROOT/hack/render-cloud-manifest.sh" \
	"$REPO_ROOT/deploy/cloud/karpenter/nodepool.yaml" "$CLOUD_CLUSTER" "$karpenter_node_role" | kc apply -f -
kc apply -f "$REPO_ROOT/deploy/cloud/storage"

# 4. Argo CD, the Secrets, the root Application and the provisioning are the
# same script the local stack runs.
echo "The cluster is ready. Running the shared stack bootstrap."
bash "$REPO_ROOT/hack/bootstrap-stack.sh"

if [ -n "${PAGERLESS_DASHBOARD_BASE_URL:-}" ] || [ -n "${PAGERLESS_GRAFANA_URL:-}" ]; then
	kc -n argocd patch application root --type=merge \
		-p '{"spec":{"syncPolicy":{"automated":{"selfHeal":false}}}}' >/dev/null 2>&1 || true
	kc -n argocd patch application pagerless-server --type=merge \
		-p '{"spec":{"syncPolicy":{"automated":{"selfHeal":false}}}}' >/dev/null 2>&1 || true
	server_args_patch="$(
		kc -n pagerless get deployment pagerless-server -o json |
			uv run --no-project --python "$PYTHON_VERSION" -c '
import json, os, sys
dep = json.load(sys.stdin)
args = dep["spec"]["template"]["spec"]["containers"][0]["args"]
dashboard_url = os.environ.get("PAGERLESS_DASHBOARD_BASE_URL", "")
grafana_url = os.environ.get("PAGERLESS_GRAFANA_URL", "")
updated = []
for arg in args:
    if dashboard_url and arg.startswith("--dashboard-base-url="):
        updated.append(f"--dashboard-base-url={dashboard_url}")
    elif grafana_url and arg.startswith("--grafana-url="):
        updated.append(f"--grafana-url={grafana_url}")
    else:
        updated.append(arg)
print(json.dumps({"spec": {"template": {"spec": {"containers": [{"name": "server", "args": updated}]}}}}))
'
	)"
	kc -n pagerless patch deployment pagerless-server --type=strategic --patch "$server_args_patch"
	kc -n pagerless rollout status deployment/pagerless-server --timeout=180s
fi

cat <<EOF

The EKS session is up in ${CLOUD_REGION}. Every Argo CD Application is Synced and Healthy.

The dashboard and Grafana are not exposed; reach them locally with a port-forward:
  kubectl --context ${CLOUD_CONTEXT} -n pagerless port-forward svc/pagerless-server 8080:8082
  kubectl --context ${CLOUD_CONTEXT} -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80

For Tailscale access from another device (for example http://devbox:8080 and http://devbox:3000):
  kubectl --context ${CLOUD_CONTEXT} -n pagerless port-forward --address 0.0.0.0 svc/pagerless-server 8080:8082
  kubectl --context ${CLOUD_CONTEXT} -n monitoring port-forward --address 0.0.0.0 svc/kube-prometheus-stack-grafana 3000:80

Read the provisioned Dashboard operator credentials from Secret pagerless/pagerless-dashboard-operator:
  kubectl --context ${CLOUD_CONTEXT} -n pagerless get secret pagerless-dashboard-operator -o jsonpath='{.data.username}' | base64 --decode; echo
  kubectl --context ${CLOUD_CONTEXT} -n pagerless get secret pagerless-dashboard-operator -o jsonpath='{.data.password}' | base64 --decode; echo

List node instance IDs and open a shell inside an EKS node via AWS Systems Manager Session Manager:
  kubectl --context ${CLOUD_CONTEXT} get nodes -o custom-columns=NAME:.metadata.name,INSTANCE:.spec.providerID
  AWS_PROFILE=pagerless aws ssm start-session --target <instance-id> --region ${CLOUD_REGION}

Break the Sandbox, and drive the CLI, against this cluster by exporting the context:
  export PAGERLESS_KUBE_CONTEXT=${CLOUD_CONTEXT}
  make break SCENARIO=oom

Destroy the session, and prove that nothing billable remains, with:
  make cloud-down
EOF
