#!/usr/bin/env bash
# Destroys one EKS session and proves that nothing billable remains.
#
# Two kinds of resource are outside Terraform's state and would survive a
# `terraform destroy` while still billing: the EC2 instances Karpenter launched
# and the EBS volumes the CSI driver provisioned. Both are removed first, by
# deleting the NodePools and the claims and waiting for each to finish.
#
# The Terraform state bucket and the two GitHub Actions roles are the only
# resources this command leaves behind, by design: the bucket outlives every
# session, and the nightly teardown depends on the roles.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions
load_env

CLOUD_ACCOUNT_ID="${PAGERLESS_CLOUD_ACCOUNT_ID:-207567775100}"
CLOUD_REGION="${AWS_REGION:-us-east-1}"
CLOUD_CLUSTER="${PAGERLESS_CLOUD_CLUSTER:-pagerless}"
CLOUD_CONTEXT="${PAGERLESS_CLOUD_CONTEXT:-pagerless-eks}"

export AWS_REGION="$CLOUD_REGION"

require_commands terraform aws kubectl

account="$(aws sts get-caller-identity --query Account --output text)"
[ "$account" = "$CLOUD_ACCOUNT_ID" ] ||
	die "this teardown must run in account ${CLOUD_ACCOUNT_ID}, but the credentials are for ${account}."

# The kubeconfig entry is named after the cluster's ARN, so compute it before
# the state is destroyed and the outputs are gone.
cluster_arn="arn:aws:eks:${CLOUD_REGION}:${CLOUD_ACCOUNT_ID}:cluster/${CLOUD_CLUSTER}"

# 1. Remove what Terraform cannot see. Each step tolerates an absent cluster,
# because a failed session may already have lost its control plane and the
# destroy below must still run.
if kubectl --context "$CLOUD_CONTEXT" get nodes >/dev/null 2>&1; then
	echo "Removing the Karpenter capacity and the persistent claims before the destroy."

	# Deleting the NodePool stops new provisioning; deleting the NodeClaims drains
	# and terminates the instances. Karpenter's finalizer returns only after the
	# instance is gone.
	kubectl --context "$CLOUD_CONTEXT" delete nodepool --all --ignore-not-found >/dev/null 2>&1 || true
	kubectl --context "$CLOUD_CONTEXT" delete nodeclaims --all --ignore-not-found --wait=true --timeout=20m || true

	# Deleting a claim deletes its volume, because the StorageClass reclaim policy
	# is Delete. The wait blocks on the protection finalizers, which the CSI
	# driver removes only after the volume is gone.
	kubectl --context "$CLOUD_CONTEXT" delete pvc --all --all-namespaces --ignore-not-found --wait=true --timeout=20m || true
fi

# 2. Destroy the stack. An empty state is success: this is also what the nightly
# teardown runs.
terraform -chdir="$REPO_ROOT/infra" init -input=false
terraform -chdir="$REPO_ROOT/infra" destroy -input=false -auto-approve

# 3. Remove the session's kubeconfig entries, so a later command cannot silently
# target a cluster that no longer exists.
kubectl config delete-context "$CLOUD_CONTEXT" >/dev/null 2>&1 || true
kubectl config unset "clusters.${cluster_arn}" >/dev/null 2>&1 || true
kubectl config unset "users.${cluster_arn}" >/dev/null 2>&1 || true

# 4. Prove that nothing billable remains. The check retries for a bounded time,
# because AWS deletes asynchronously.
bash "$REPO_ROOT/hack/cloud-teardown-check.sh"

echo
echo "The EKS session is destroyed and the teardown check passed."
echo "The state bucket and the two GitHub Actions roles remain, as designed."
