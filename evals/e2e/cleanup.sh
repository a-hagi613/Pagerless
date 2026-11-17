# shellcheck shell=bash
# Sourced by the E2E runner; all variables and PIDs belong to that one run.
cleanup_e2e() {
	local result="$1" preserve_state=0 process
	for process in "${observer:-}" "${forward:-}"; do
		if [ -n "$process" ]; then
			kill "$process" 2>/dev/null || true
			wait "$process" 2>/dev/null || true
		fi
	done
	if [ "$keep" = 1 ]; then
		if [ "$result" != 0 ]; then echo "Retained failed E2E state in $state for diagnosis." >&2; fi
		return "$result"
	fi
	if [ "$created" = 1 ]; then
		if ! kind delete cluster --name "$KIND_CLUSTER_NAME" >/dev/null 2>&1; then
			echo "Could not delete the E2E cluster; retained restricted state in $state." >&2
			preserve_state=1
			result=1
		fi
	elif [ "${policies_saved:-0}" = 1 ]; then
		if ! bash "$script_dir/restore.sh" "$state"; then
			echo "Could not restore Argo CD policies; retained recovery files in $state." >&2
			preserve_state=1
			result=1
		fi
	fi
	if [ "$state_is_temporary" = 1 ] && [ "$preserve_state" = 0 ]; then rm -rf "$state"; fi
	return "$result"
}
