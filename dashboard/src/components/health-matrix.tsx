import { Cpu, Network, Server, Zap } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { HealthPill, type Health } from "@/components/pills";
import type { PortalFilters } from "@/lib/filters";
import type { ClusterMatrix, NamespaceHealth, NodeHealth } from "@/lib/types";
import { cn } from "@/lib/utils";

function namespaceHealth(namespace: NamespaceHealth): Health {
  return namespace.status;
}

function workloadDot(status: "healthy" | "degraded" | "critical"): string {
  if (status === "critical") {
    return "bg-destructive";
  }
  if (status === "degraded") {
    return "bg-warning";
  }
  return "bg-primary";
}

function NodeRow({
  node,
  selected,
  onToggle,
}: {
  node: NodeHealth;
  selected: boolean;
  onToggle: () => void;
}) {
  return (
    <li>
      <button
        type="button"
        onClick={onToggle}
        aria-pressed={selected}
        className={cn(
          "flex w-full items-center gap-2 rounded px-1 py-0.5 text-left font-mono text-xs transition-colors hover:bg-accent",
          selected && "bg-primary-soft text-primary-soft-foreground",
        )}
      >
        <Cpu className="size-3 shrink-0 text-muted-foreground" aria-hidden="true" />
        <span className="min-w-0 flex-1 truncate">{node.name}</span>
        <span className="w-9 text-right tabular-nums">{node.cpuPercent}%</span>
        <span
          className={cn(
            "w-14 text-right",
            node.status === "pressure" ? "font-semibold text-warning" : "text-muted-foreground",
          )}
        >
          {node.status === "pressure" ? "pressure" : "ready"}
        </span>
      </button>
    </li>
  );
}

function NamespaceTile({
  namespace,
  selected,
  onToggle,
}: {
  namespace: NamespaceHealth;
  selected: boolean;
  onToggle: () => void;
}) {
  return (
    <div
      className={cn(
        "flex min-w-0 flex-col rounded-xl border bg-card transition-colors",
        selected ? "border-primary ring-1 ring-primary" : "border-border",
      )}
    >
      <button
        type="button"
        onClick={onToggle}
        aria-pressed={selected}
        aria-label={`Filter the inbox by namespace ${namespace.name}`}
        className="flex items-center gap-2 rounded-t-xl px-3 py-2 text-left transition-colors hover:bg-accent"
      >
        <span className={cn("size-2 shrink-0 rounded-full", workloadDot(namespace.status))} aria-hidden="true" />
        <span className="min-w-0 flex-1">
          <span className="block truncate font-mono text-sm text-foreground">{namespace.name}</span>
          <span className="block font-mono text-xs text-muted-foreground">
            {namespace.pods.ready}/{namespace.pods.total} pods · {namespace.counts.deployments} deploy ·{" "}
            {namespace.counts.cronJobs} cron
          </span>
        </span>
        <HealthPill health={namespaceHealth(namespace)} />
      </button>
      <ul className="flex flex-1 flex-col gap-1 border-t border-border-subtle p-2">
        {namespace.workloads.map((workload) => (
          <li key={workload.name} className="flex items-center gap-1.5 px-1 py-0.5 font-mono text-xs">
            <span className={cn("size-1.5 shrink-0 rounded-sm", workloadDot(workload.status))} aria-hidden="true" />
            <span className="min-w-0 flex-1 truncate text-muted-foreground">{workload.name}</span>
            <span className="text-muted-foreground tabular-nums">{workload.ready}</span>
            <span className="sr-only">
              {workload.status === "healthy" ? "healthy" : workload.status === "degraded" ? "degraded" : "critical"}
            </span>
          </li>
        ))}
        {namespace.workloads.length === 0 && <li className="px-1 text-xs text-muted-foreground">no workloads observed</li>}
      </ul>
    </div>
  );
}

// The matrix is the navigation control, not an illustration: a tile is the filter for its scope, and the
// brand lockup clears it.
export function HealthMatrix({
  matrix,
  filters,
  onToggleNamespace,
  onToggleNode,
  className,
}: {
  matrix: ClusterMatrix;
  filters: PortalFilters;
  onToggleNamespace: (namespace: string) => void;
  onToggleNode: (node: string) => void;
  className?: string;
}) {
  const pressureNodes = matrix.nodesDetail.filter((node) => node.status === "pressure").length;

  return (
    <section aria-labelledby="cluster-health-heading" className={cn("flex flex-col gap-2", className)}>
      <div className="flex flex-wrap items-center gap-2">
        <h1 id="cluster-health-heading" className="text-xl font-semibold tracking-tight text-balance text-foreground">
          Cluster health
        </h1>
        <Badge variant="muted">{matrix.context}</Badge>
        <span className="font-mono text-xs text-muted-foreground">{matrix.version}</span>
        <span className="ml-auto flex flex-wrap items-center gap-3 font-mono text-xs text-muted-foreground">
          <span className="inline-flex items-center gap-1">
            <Server className="size-3" aria-hidden="true" /> {matrix.pods.ready}/{matrix.pods.total} pods
          </span>
          <span className="inline-flex items-center gap-1">
            <Cpu className="size-3" aria-hidden="true" /> {matrix.nodes} nodes
          </span>
          <span className="inline-flex items-center gap-1 text-primary">
            <Zap className="size-3" aria-hidden="true" /> {matrix.merged} merged
          </span>
        </span>
      </div>

      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 xl:grid-cols-4">
        {matrix.namespaces.map((namespace) => (
          <NamespaceTile
            key={namespace.name}
            namespace={namespace}
            selected={filters.namespace === namespace.name}
            onToggle={() => {
              onToggleNamespace(namespace.name);
            }}
          />
        ))}

        <div
          className={cn(
            "flex min-w-0 flex-col rounded-xl border bg-card transition-colors",
            filters.node !== null ? "border-primary ring-1 ring-primary" : "border-border",
          )}
        >
          <div className="flex items-center gap-2 px-3 py-2">
            <Network className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
            <span className="min-w-0 flex-1">
              <span className="block font-mono text-sm text-foreground">nodes</span>
              <span className="block font-mono text-xs text-muted-foreground">
                {pressureNodes} pressure · {matrix.nodesDetail.length} nodes
              </span>
            </span>
            <HealthPill health={pressureNodes > 0 ? "degraded" : "healthy"} />
          </div>
          <ul className="flex flex-1 flex-col gap-0.5 border-t border-border-subtle p-2">
            {matrix.nodesDetail.map((node) => (
              <NodeRow
                key={node.name}
                node={node}
                selected={filters.node === node.name}
                onToggle={() => {
                  onToggleNode(node.name);
                }}
              />
            ))}
            {matrix.nodesDetail.length === 0 && (
              <li className="px-1 text-xs text-muted-foreground">node read is unavailable in this watch mode</li>
            )}
          </ul>
        </div>
      </div>
    </section>
  );
}
