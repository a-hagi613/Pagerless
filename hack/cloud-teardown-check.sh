#!/usr/bin/env bash
# Fails when a session left something billable behind.
#
# The session's resources all carry project=pagerless, so the Resource Groups
# Tagging API is the one place that sees every service at once. EBS volumes are
# also queried directly, because a volume is the resource that has survived a
# real teardown: the tagging index can lag, and a volume whose tag was lost is
# still billed per GiB-month. Three kinds of resource are expected to survive a
# teardown:
#
#   - the Terraform state bucket, which outlives every session by design;
#   - the two GitHub Actions roles and the OIDC provider they trust, which the
#     nightly teardown itself runs as;
#   - the cluster's KMS key, while it waits for its deletion window, which is
#     reported with the charge it still carries instead of being ignored.
#
# A failed AWS query fails the check. An account that cannot be read is not an
# empty account, and reading an error as "nothing left" is how a volume escaped
# a teardown.
#
# AWS deletes asynchronously, so the check retries for a bounded time before it
# gives up. It never retries past the limit: a resource that is still there
# after five minutes is a teardown failure, not a slow API.
set -euo pipefail

REGION="${AWS_REGION:-us-east-1}"
export AWS_REGION="$REGION"
ACCOUNT="207567775100"
STATE_BUCKET="pagerless-terraform-state-207567775100"

ATTEMPTS="${TEARDOWN_CHECK_ATTEMPTS:-10}"
INTERVAL_SECONDS="${TEARDOWN_CHECK_INTERVAL_SECONDS:-30}"

exempt=(
	"arn:aws:s3:::${STATE_BUCKET}"
	"arn:aws:iam::${ACCOUNT}:role/pagerless-github-plan"
	"arn:aws:iam::${ACCOUNT}:role/pagerless-github-destroy"
	"arn:aws:iam::${ACCOUNT}:oidc-provider/token.actions.githubusercontent.com"
)

is_exempt() {
	local candidate="$1" allowed
	for allowed in "${exempt[@]}"; do
		[ "$candidate" = "$allowed" ] && return 0
	done
	return 1
}

# A KMS key that is scheduled for deletion still exists for up to a week, and it
# bills USD 1 per month until the deletion completes. That is expected after a
# teardown; an enabled key is not.
is_pending_deletion() {
	local arn="$1" state
	case "$arn" in
	arn:aws:kms:*) ;;
	*) return 1 ;;
	esac
	state="$(aws kms describe-key --region "$REGION" --key-id "$arn" \
		--query 'KeyMetadata.KeyState' --output text 2>/dev/null || true)"
	[ "$state" = "PendingDeletion" ]
}

# tagged_resources prints every tagged resource that is not one of the persistent
# ones. A failed query returns non-zero instead of printing nothing.
tagged_resources() {
	local output
	output="$(aws resourcegroupstaggingapi get-resources \
		--region "$REGION" \
		--tag-filters Key=project,Values=pagerless \
		--query 'ResourceTagMappingList[].ResourceARN' \
		--output text)" || return 1
	printf '%s\n' "$output" |
		# The AWS CLI on Windows ends lines with CRLF; a carriage return would
		# defeat every comparison below.
		tr -d '\r' |
		tr '\t' '\n' |
		while IFS= read -r arn; do
			[ -n "$arn" ] || continue
			if ! is_exempt "$arn"; then
				printf '%s\n' "$arn"
			fi
		done
}

# ebs_resource_arns prints the ARNs of the session's EBS volumes: the tagged ones
# and the ones the CSI driver created. A volume whose deletion is still running
# is reported until EC2 has removed it.
ebs_resource_arns() {
	local tagged csi id
	tagged="$(aws ec2 describe-volumes --region "$REGION" \
		--filters "Name=tag:project,Values=pagerless" \
		--query 'Volumes[].VolumeId' --output text)" || return 1
	csi="$(aws ec2 describe-volumes --region "$REGION" \
		--filters "Name=tag-key,Values=kubernetes.io/created-for/pv/name" \
		--query 'Volumes[].VolumeId' --output text)" || return 1
	printf '%s\n%s\n' "$tagged" "$csi" | tr -d '\r' | tr '\t' '\n' |
		while IFS= read -r id; do
			[ -n "$id" ] || continue
			printf 'arn:aws:ec2:%s:%s:volume/%s\n' "$REGION" "$ACCOUNT" "$id"
		done
}

attempt=1
while :; do
	leftovers=()
	pending_keys=()
	query_failure=0
	tagged=""
	volumes=""
	tagged="$(tagged_resources)" || {
		query_failure=1
		tagged=""
	}
	volumes="$(ebs_resource_arns)" || {
		query_failure=1
		volumes=""
	}
	while IFS= read -r arn; do
		[ -n "$arn" ] || continue
		if is_pending_deletion "$arn"; then
			pending_keys+=("$arn")
		else
			leftovers+=("$arn")
		fi
	done < <(printf '%s\n%s\n' "$tagged" "$volumes" | awk 'NF' | sort -u)

	if [ "$query_failure" -eq 0 ] && [ "${#leftovers[@]}" -eq 0 ]; then
		echo "The teardown check passed: no billable resource tagged project=pagerless remains."
		if [ "${#pending_keys[@]}" -gt 0 ]; then
			printf '%s\n' "${pending_keys[@]}" | sed 's/^/  waiting for deletion: /'
			echo "Those keys bill USD 1 per month until their deletion window closes; they are not a new charge from the session."
		fi
		exit 0
	fi
	if [ "$attempt" -ge "$ATTEMPTS" ]; then
		echo "The teardown check failed: the account could not be proven clean." >&2
		if [ "$query_failure" -ne 0 ]; then
			echo "  an AWS query failed on every attempt, so nothing here says the resources are gone" >&2
		fi
		if [ "${#leftovers[@]}" -gt 0 ]; then
			echo "  these resources tagged project=pagerless or created by the CSI driver still exist:" >&2
			printf '%s\n' "${leftovers[@]}" | sed 's/^/    /' >&2
		fi
		echo "Destroy them, or record why they must persist, before the session ends." >&2
		exit 1
	fi
	echo "Attempt ${attempt}/${ATTEMPTS}: waiting ${INTERVAL_SECONDS}s for the remaining resources to be deleted."
	if [ "$query_failure" -ne 0 ]; then
		echo "  an AWS query failed on this attempt; retrying." >&2
	fi
	sleep "$INTERVAL_SECONDS"
	attempt=$((attempt + 1))
done
