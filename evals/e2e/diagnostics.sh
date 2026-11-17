#!/usr/bin/env bash
# Sourced by the E2E runner. Writes the evidence a failed run needs, without
# reading Secrets or printing credentials.
dump_e2e_diagnostics() {
	local context="kind-${KIND_CLUSTER_NAME}"
	local directory="${PAGERLESS_E2E_DIAGNOSTICS:-${state}/diagnostics}"
	mkdir -p "$directory"
	echo "Writing end-to-end diagnostics to $directory" >&2
	kubectl --context "$context" -n pagerless get triages -o yaml >"$directory/triages.yaml" 2>&1
	kubectl --context "$context" -n pagerless get jobs -o yaml >"$directory/jobs.yaml" 2>&1
	kubectl --context "$context" -n pagerless get pods -o wide >"$directory/pagerless-pods.txt" 2>&1
	kubectl --context "$context" -n sandbox get pods -o wide >"$directory/sandbox-pods.txt" 2>&1
	kubectl --context "$context" -n sandbox get events --sort-by=.lastTimestamp >"$directory/sandbox-events.txt" 2>&1
	kubectl --context "$context" -n pagerless get events --sort-by=.lastTimestamp >"$directory/pagerless-events.txt" 2>&1
	kubectl --context "$context" -n argocd get applications >"$directory/argocd-applications.txt" 2>&1
	kubectl --context "$context" -n pagerless logs deployment/pagerless-server --all-containers=true --tail=400 \
		>"$directory/pagerless-server.log" 2>&1
	kubectl --context "$context" -n pagerless logs deployment/mcp-grafana --all-containers=true --tail=200 \
		>"$directory/mcp-grafana.log" 2>&1
	kubectl --context "$context" -n agentgateway-system get pods -o wide >"$directory/gateway-pods.txt" 2>&1
	kubectl --context "$context" -n agentgateway-system logs deployment/agentgateway-proxy --all-containers=true \
		--tail=400 >"$directory/gateway-proxy.log" 2>&1
	kubectl --context "$context" -n agentgateway-system get agentgatewaybackends,agentgatewaypolicies,httproutes \
		-o yaml >"$directory/gateway-config.yaml" 2>&1
	for job in $(kubectl --context "$context" -n pagerless get jobs -o name 2>/dev/null); do
		name="${job#job.batch/}"
		kubectl --context "$context" -n pagerless logs "$job" --all-containers=true --tail=400 \
			>"$directory/agent-${name}.log" 2>&1
	done
	kubectl --context "$context" -n pagerless describe pods >"$directory/pagerless-pods.txt.describe" 2>&1
}
