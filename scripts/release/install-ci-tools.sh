#!/usr/bin/env bash
# Installs the exact Helm, kubectl, and Krew binaries the release job uses into
# one pinned path. Each download is checked against hack/versions.env, and the
# caller puts this directory on PATH. Nothing is installed system-wide, so the
# same script works on a laptop and on a CI runner.
#
# Usage: install-ci-tools.sh [--dir DIR]
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../hack/lib.sh"
load_versions

tools_dir="${RELEASE_CI_TOOLS_DIR:-$REPO_ROOT/bin/ci-tools}"

usage() {
	cat >&2 <<'EOF'
Usage: install-ci-tools.sh [--dir DIR]

  --dir DIR  Install the tools here. Default: bin/ci-tools
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--dir)
		tools_dir="${2:?--dir needs a directory}"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage
		die "unknown argument $1"
		;;
	esac
done

require_commands curl sha256sum tar install
mkdir -p "$tools_dir"
work="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-ci-tools.XXXXXXXX")"
trap 'rm -rf "$work"' EXIT

fetch_helm() {
	curl --fail --silent --show-error --location \
		"https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz" --output "$work/helm.tar.gz"
	curl --fail --silent --show-error --location \
		"https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz.sha256sum" --output "$work/helm.sha256sum"
	echo "$(awk '{print $1}' "$work/helm.sha256sum")  $work/helm.tar.gz" | sha256sum --check --status
	tar -xzf "$work/helm.tar.gz" -C "$work" linux-amd64/helm
	install -m 0755 "$work/linux-amd64/helm" "$tools_dir/helm"
}

fetch_kubectl() {
	curl --fail --silent --show-error --location \
		"https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" --output "$work/kubectl"
	curl --fail --silent --show-error --location \
		"https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl.sha256" --output "$work/kubectl.sha256"
	echo "$(cat "$work/kubectl.sha256")  $work/kubectl" | sha256sum --check --status
	install -m 0755 "$work/kubectl" "$tools_dir/kubectl"
}

fetch_helm
fetch_kubectl
RELEASE_TOOLS_DIR="$tools_dir" bash "$REPO_ROOT/scripts/release/tools.sh" krew
# kubectl discovers plugins as kubectl-<name>; Krew ships as `krew` and
# `kubectl krew` must resolve through this pinned link.
ln -sf krew "$tools_dir/kubectl-krew"

"$tools_dir/helm" version --short
"$tools_dir/kubectl" version --client
"$tools_dir/krew" version
echo "release tools are in $tools_dir"
