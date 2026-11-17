#!/usr/bin/env bash
# Fails when a session left something billable behind.
#
# The session's resources all carry project=pagerless, so the Resource Groups
# Tagging API is the one place that sees every service at once. Three kinds of
# resource are expected to survive a teardown:
#
#   - the Terraform state bucket, which outlives every session by design;
#   - the two GitHub Actions roles and the OIDC provider they trust, which the
#     nightly teardown itself runs as;
#   - the cluster's KMS key, while it waits for its deletion window, which is
#     reported with the charge it still carries instead of being ignored.
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

# Prints every tagged resource that is not one of the persistent ones.
tagged_resources() {
	local arn
	aws resourcegroupstaggingapi get-resources \
		--region "$REGION" \
		--tag-filters Key=project,Values=pagerless \
		--query 'ResourceTagMappingList[].ResourceARN' \
		--output text 2>/dev/null |
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

attempt=1
while :; do
	leftovers=()
	pending_keys=()
	while IFS= read -r arn; do
		[ -n "$arn" ] || continue
		if is_pending_deletion "$arn"; then
			pending_keys+=("$arn")
		else
			leftovers+=("$arn")
		fi
	done < <(tagged_resources)

	if [ "${#leftovers[@]}" -eq 0 ]; then
		echo "The teardown check passed: no billable resource tagged project=pagerless remains."
		if [ "${#pending_keys[@]}" -gt 0 ]; then
			printf '%s\n' "${pending_keys[@]}" | sed 's/^/  waiting for deletion: /'
			echo "Those keys bill USD 1 per month until their deletion window closes; they are not a new charge from the session."
		fi
		exit 0
	fi
	if [ "$attempt" -ge "$ATTEMPTS" ]; then
		echo "The teardown check failed: these resources tagged project=pagerless still exist:" >&2
		printf '%s\n' "${leftovers[@]}" | sed 's/^/  /' >&2
		echo "Destroy them, or record why they must persist, before the session ends." >&2
		exit 1
	fi
	echo "Attempt ${attempt}/${ATTEMPTS}: waiting ${INTERVAL_SECONDS}s for the remaining resources to be deleted."
	sleep "$INTERVAL_SECONDS"
	attempt=$((attempt + 1))
done
