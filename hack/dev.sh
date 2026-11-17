#!/usr/bin/env bash
# Builds the Pagerless runtime images, then asks the standard bootstrap to load
# those exact references into kind before Argo CD deploys them.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
if ! docker info >/dev/null 2>&1 && command -v docker.exe >/dev/null 2>&1; then
	docker_wrapper_dir="$(mktemp -d)"
	trap 'rm -rf "$docker_wrapper_dir"' EXIT
	docker_exe="$(command -v docker.exe)"
	printf '#!/usr/bin/env sh\nexec "%s" "$@"\n' "$docker_exe" >"$docker_wrapper_dir/docker"
	chmod 700 "$docker_wrapper_dir/docker"
	PATH="$docker_wrapper_dir:$PATH"
	export PATH
fi

make -C "$repo_root" build-images
if [ "${PAGERLESS_E2E:-0}" = "1" ]; then
	. "$script_dir/versions.env"
	docker build --file "$repo_root/Dockerfile.pagerless-e2e-adapter" \
		--build-arg "VCS_REF=${PAGERLESS_BUILD_REVISION:-local}" \
		--tag "$PAGERLESS_E2E_ADAPTER_IMAGE" "$repo_root"
	docker build --file "$repo_root/Dockerfile.pagerless-agent-e2e" \
		--build-arg "BASE_IMAGE=$PAGERLESS_AGENT_IMAGE" \
		--tag "$PAGERLESS_AGENT_IMAGE" "$repo_root"
fi
PAGERLESS_LOCAL_IMAGES=1 bash "$script_dir/up.sh"
