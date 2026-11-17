#!/usr/bin/env bash
# Reports what one EKS session cost, from Cost Explorer.
#
# Run it by hand, once, after the session's data has settled: Cost Explorer lags
# up to about 24 hours, and AWS bills USD 0.01 for every GetCostAndUsage request.
# For that reason the script makes exactly one grouped request, and nothing in CI
# or on a schedule calls it. The README quotes the output.
#
# Usage:
#   hack/session-cost.sh --start 2026-11-18
#
# The filter is the project=pagerless cost-allocation tag, which the account
# keeps activated outside Terraform (ADR-0024). A session's resources carry it,
# so the figure is the session rather than the whole account.
set -euo pipefail

REGION="${AWS_REGION:-us-east-1}"
export AWS_REGION="$REGION"

start=""
end=""

while [ $# -gt 0 ]; do
	case "$1" in
	--start)
		start="${2:?--start needs a date}"
		shift 2
		;;
	--end)
		end="${2:?--end needs a date}"
		shift 2
		;;
	*)
		echo "usage: $0 [--start YYYY-MM-DD] [--end YYYY-MM-DD]" >&2
		exit 2
		;;
	esac
done

command -v aws >/dev/null 2>&1 || {
	echo "error: the AWS CLI is required." >&2
	exit 1
}

# Cost Explorer's end date is exclusive, so the default window is the one day the
# session ran. GNU date and BSD date disagree on relative dates, so both forms
# are tried.
start="${start:-$(date -u +%Y-%m-%d)}"
if [ -z "$end" ]; then
	end="$(date -u -d "${start} + 1 day" +%Y-%m-%d 2>/dev/null || date -u -v+1d -j -f %Y-%m-%d "$start" +%Y-%m-%d)"
fi

echo "Cost Explorer, project=pagerless, ${start} to ${end} (exclusive)."
echo "One request: it bills USD 0.01, and the data can lag up to 24 hours."

rows="$(aws ce get-cost-and-usage \
	--time-period "Start=${start},End=${end}" \
	--granularity DAILY \
	--metrics UnblendedCost \
	--filter '{"Tags":{"Key":"project","Values":["pagerless"]}}' \
	--group-by Type=DIMENSION,Key=SERVICE \
	--query 'ResultsByTime[].Groups[].[Keys[0],Metrics.UnblendedCost.Amount]' \
	--output text)"

if [ -z "$rows" ]; then
	echo "No cost data for the filter in that window. Either the tag is not active yet or the window has not settled."
	exit 0
fi

printf '%s\n' "$rows" | awk -F'\t' '
	{ printf "  %-48s %8.4f USD\n", $1, $2; total += $2 }
	END { printf "  %-48s %8.4f USD\n", "total", total }
'
