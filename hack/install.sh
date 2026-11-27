#!/usr/bin/env bash
# Installs Pagerless from charts/pagerless. Helm is the only installation entry
# point; this script only assembles the documented values.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

RELEASE="pagerless"
NAMESPACE="pagerless"
CHART="$REPO_ROOT/charts/pagerless"
CONTEXT=""
DRY_RUN=0
SECRET=""
MODEL_BASE_URL=""
MODEL_NAME=""
GITHUB_REPOSITORY=""
GITHUB_BASE_BRANCH="main"
GITHUB_PATH_PREFIX=""
EXTRA=()

usage() {
	cat <<'EOF'
Usage: bash hack/install.sh --secret NAME --model-base-url URL --model-name NAME \
         --github-repository OWNER/NAME [options]

The Secret must already exist and carry the keys named in charts/pagerless/values.yaml.

Options:
  --namespace NAME            release namespace (default pagerless)
  --release NAME              release name (default pagerless)
  --context CONTEXT           kubeconfig context
  --github-base-branch NAME   base branch (default main)
  --github-path-prefix PATH   repository directory that bounds reads and proposals
  --dry-run                   render without contacting a cluster
  --set KEY=VALUE             pass one additional Helm value (repeatable)
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--secret) SECRET="$2"; shift 2 ;;
	--model-base-url) MODEL_BASE_URL="$2"; shift 2 ;;
	--model-name) MODEL_NAME="$2"; shift 2 ;;
	--github-repository) GITHUB_REPOSITORY="$2"; shift 2 ;;
	--github-base-branch) GITHUB_BASE_BRANCH="$2"; shift 2 ;;
	--github-path-prefix) GITHUB_PATH_PREFIX="$2"; shift 2 ;;
	--namespace) NAMESPACE="$2"; shift 2 ;;
	--release) RELEASE="$2"; shift 2 ;;
	--context) CONTEXT="$2"; shift 2 ;;
	--dry-run) DRY_RUN=1; shift ;;
	--set) EXTRA+=(--set "$2"); shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option: $1" ;;
	esac
done

missing=()
[ -n "$SECRET" ] || missing+=("--secret")
[ -n "$MODEL_BASE_URL" ] || missing+=("--model-base-url")
[ -n "$MODEL_NAME" ] || missing+=("--model-name")
[ -n "$GITHUB_REPOSITORY" ] || missing+=("--github-repository")
if [ "${#missing[@]}" -ne 0 ]; then
	die "missing required setting(s): ${missing[*]}. Nothing was installed."
fi

args=(
	upgrade --install "$RELEASE" "$CHART"
	--namespace "$NAMESPACE" --create-namespace
	--set "existingSecret=${SECRET}"
	--set "model.baseUrl=${MODEL_BASE_URL}"
	--set "model.name=${MODEL_NAME}"
	--set "github.repository=${GITHUB_REPOSITORY}"
	--set "github.baseBranch=${GITHUB_BASE_BRANCH}"
)
if [ -n "$GITHUB_PATH_PREFIX" ]; then
	args+=(--set "github.pathPrefix=${GITHUB_PATH_PREFIX}")
fi
args+=("${EXTRA[@]}")
if [ -n "$CONTEXT" ]; then
	args+=(--kube-context "$CONTEXT")
fi
if [ "$DRY_RUN" -eq 1 ]; then
	args+=(--dry-run=client)
fi

helm "${args[@]}"

if [ "$DRY_RUN" -eq 1 ]; then
	exit 0
fi

echo
echo "Pagerless is installed in namespace ${NAMESPACE}."
echo "The private Service is ${RELEASE}-server:8080. Reach it with:"
echo "  kubectl --namespace ${NAMESPACE} port-forward service/${RELEASE}-server 8080:8080"
echo "Then open http://localhost:8080 and sign in with the operator token from Secret ${SECRET}."
echo "Readiness names any unmet prerequisite:"
echo "  kubectl --namespace ${NAMESPACE} port-forward service/${RELEASE}-server 8080:8080 &"
echo "  curl --silent http://localhost:8080/readyz"
