#!/usr/bin/env bash
# Destroys one EKS session and proves that nothing billable remains.
#
# Two kinds of resource are outside Terraform's state and would survive a
# `terraform destroy` while still billing: the EC2 instances Karpenter launched
# and the EBS volumes the CSI driver provisioned. Both are removed first, by
# deleting the NodePools and the claims and waiting for each to finish; the
# volumes are then swept by tag, which also covers a cluster that is already
# gone.
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
	echo "Stopping Argo CD before removing anything it manages."

	# The Applications sync with automated prune and selfHeal, so a namespace or
	# claim deleted while they exist is recreated within seconds. Delete the root
	# Application first, with a foreground cascade, which removes the child
	# Applications it manages, then any that remain, and refuse to continue until
	# none is left: everything below assumes nothing will reconcile.
	kc -n argocd delete application root --cascade=foreground --ignore-not-found --wait=true --timeout=10m >/dev/null 2>&1 || true
	kc -n argocd get applications -o name 2>/dev/null | while IFS= read -r application; do
		[ -n "$application" ] || continue
		kc -n argocd delete "$application" --cascade=foreground --ignore-not-found --wait=false >/dev/null 2>&1 || true
	done
	for _ in $(seq 1 60); do
		[ -z "$(kc -n argocd get applications -o name 2>/dev/null)" ] && break
		sleep 5
	done
	if [ -n "$(kc -n argocd get applications -o name 2>/dev/null)" ]; then
		die "Argo CD still has Applications; they would recreate what this teardown deletes. Delete them, then retry."
	fi

	echo "Removing the Karpenter capacity and the persistent claims before the destroy."

	# Deleting the NodePool stops new provisioning; deleting the NodeClaims drains
	# and terminates the instances. Karpenter's finalizer returns only after the
	# instance is gone.
	kubectl --context "$CLOUD_CONTEXT" delete nodepool --all --ignore-not-found >/dev/null 2>&1 || true
	kubectl --context "$CLOUD_CONTEXT" delete nodeclaims --all --ignore-not-found --wait=true --timeout=20m || true

	# Delete every workload namespace before the claims. A claim whose pod still
	# exists keeps its protection finalizer, so deleting claims first would block
	# and then orphan its volume when the timeout expired.
	for namespace in $(kc get namespaces -o name); do
		case "$namespace" in
		namespace/default | namespace/kube-* | namespace/argocd) continue ;;
		esac
		kc delete "$namespace" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	done
	for _ in $(seq 1 40); do
		kc get namespaces -o name | grep -Eq 'namespace/(sandbox|pagerless|pagerless-system|monitoring|keycloak|agentgateway-system)$' || break
		sleep 15
	done

	# A claim that survives the namespace must not survive the session: an EBS
	# volume is billed per GiB-month and is invisible to Terraform's state.
	pvc_deadline=$((SECONDS + 600))
	while [ -n "$(kc get pvc --all-namespaces -o name 2>/dev/null)" ]; do
		if [ "$SECONDS" -ge "$pvc_deadline" ]; then
			# A stuck protection finalizer would otherwise leave the volume behind.
			# The namespace is already gone, so no pod can still be using it.
			kc get pvc --all-namespaces -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' |
				while IFS= read -r entry; do
					[ -n "$entry" ] || continue
					kc -n "${entry%%/*}" patch "pvc/${entry#*/}" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
				done
			kc get pv -o name | while IFS= read -r volume; do
				kc patch "$volume" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
			done
			pvc_deadline=$((SECONDS + 300))
		fi
		sleep 15
	done
	if [ -n "$(kc get pv -o name 2>/dev/null)" ]; then
		die "persistent volumes remain; destroying now could orphan an EBS volume. Inspect kubectl get pv and retry."
	fi
fi

# Karpenter creates its own instance profile, which Terraform never records. A
# profile that survives the session is an IAM leftover the teardown check would
# report, so remove the ones this cluster owns.
for profile in $(aws iam list-instance-profiles \
	--query "InstanceProfiles[?starts_with(InstanceProfileName, '${CLOUD_CLUSTER}')].InstanceProfileName" \
	--output text 2>/dev/null || true); do
	for role in $(aws iam get-instance-profile --instance-profile-name "$profile" \
		--query 'InstanceProfile.Roles[].RoleName' --output text 2>/dev/null || true); do
		aws iam remove-role-from-instance-profile --instance-profile-name "$profile" --role-name "$role" >/dev/null 2>&1 || true
	done
	aws iam delete-instance-profile --instance-profile-name "$profile" >/dev/null 2>&1 || true
done

# 2. Sweep the volumes directly. The CSI driver deletes a volume asynchronously
# when its claim is deleted, and a destroy that gets there first orphans it. The
# sweep needs no cluster: it finds the volumes by tag, so it also runs when the
# control plane was lost before this command.
bash "$REPO_ROOT/hack/cloud-volume-sweep.sh"

# 3. Destroy the stack. An empty state is success: this is also what the nightly
# teardown runs.
terraform -chdir="$REPO_ROOT/infra" init -input=false
terraform -chdir="$REPO_ROOT/infra" destroy -input=false -auto-approve

# 4. Remove the session's kubeconfig entries, so a later command cannot silently
# target a cluster that no longer exists.
kubectl config delete-context "$CLOUD_CONTEXT" >/dev/null 2>&1 || true
kubectl config unset "clusters.${cluster_arn}" >/dev/null 2>&1 || true
kubectl config unset "users.${cluster_arn}" >/dev/null 2>&1 || true

# 5. Prove that nothing billable remains. The check retries for a bounded time,
# because AWS deletes asynchronously.
bash "$REPO_ROOT/hack/cloud-teardown-check.sh"

echo
echo "The EKS session is destroyed and the teardown check passed."
echo "The state bucket and the two GitHub Actions roles remain, as designed."
