import { Cpu, GitPullRequest, Layers, Network, Server } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { HealthPill, type Health } from "@/components/pills";
import type { PortalFilters } from "@/lib/filters";
import type { ClusterHealth, NamespaceHealth, NodeHealth } from "@/lib/matrix";
import { phaseLabel, severityLabel } from "@/lib/format";
import type { Severity } from "@/lib/types";
import { cn } from "@/lib/utils";

function healthOf(severity: Severity): Health {
  if (severity === "critical") {
    return "critical";
  }
  if (severity === "warning") {
    return "degraded";
  }
  return "healthy";
}

function severityDot(severity: Severity): string {
  if (severity === "critical") {
    return "bg-destructive";
  }
  if (severity === "warning") {
    return "bg-warning";
  }
  return "bg-primary";
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
        <span className={cn("size-2 shrink-0 rounded-full", severityDot(namespace.severity))} aria-hidden="true" />
        <span className="min-w-0 flex-1">
          <span className="block truncate font-mono text-sm text-foreground">{namespace.name}</span>
          <span className="block text-xs text-muted-foreground">
            {namespace.pages.length} {namespace.pages.length === 1 ? "Page" : "Pages"} ·{" "}
            {namespace.counts.critical} critical · {namespace.counts.warning} warning
            {namespace.counts.warning === 1 ? "" : "s"}
          </span>
        </span>
        <HealthPill health={healthOf(namespace.severity)} />
      </button>
      <ul className="flex flex-1 flex-col gap-1 border-t border-border-subtle p-2">
        {namespace.workloads.map((workload) => (
          <li key={workload.page} className="flex items-center gap-1.5 px-1 py-0.5 text-xs">
            <span
              className={cn("size-1.5 shrink-0 rounded-sm", severityDot(workload.severity))}
              aria-hidden="true"
            />
            <span className="min-w-0 flex-1 truncate font-mono text-muted-foreground" title={`${workload.kind}/${workload.name}`}>
              {workload.name}
            </span>
            <span className="shrink-0 text-muted-foreground">{phaseLabel(workload.phase)}</span>
            <span className="sr-only">
              {severityLabel(workload.severity)} severity, {phaseLabel(workload.phase)}
            </span>
          </li>
        ))}
        {namespace.workloads.length === 0 && (
          <li className="px-1 text-xs text-muted-foreground">no Pages in this namespace</li>
        )}
      </ul>
    </div>
  );
}

function NodeRow({ node, selected, onToggle }: { node: NodeHealth; selected: boolean; onToggle: () => void }) {
  return (
    <li>
      <button
        type="button"
        onClick={onToggle}
        aria-pressed={selected}
        aria-label={`Filter the inbox by node ${node.name}`}
        className={cn(
          "flex w-full items-center gap-2 rounded px-1 py-1 text-left font-mono text-xs transition-colors hover:bg-accent",
          selected && "bg-primary-soft text-primary-soft-foreground",
        )}
      >
        <span className={cn("size-1.5 shrink-0 rounded-sm", severityDot(node.severity))} aria-hidden="true" />
        <span className="min-w-0 flex-1 truncate">{node.name}</span>
        <span className="shrink-0 text-muted-foreground">
          {node.pages.length} {node.pages.length === 1 ? "Page" : "Pages"}
        </span>
      </button>
    </li>
  );
}

// The matrix is the navigation control, not an illustration: a tile is the filter for its scope, and the
// brand lockup clears it. Every number on a tile is the Pages inside that scope, read from /api/pages.
export function HealthMatrix({
  health,
  version,
  filters,
  onToggleNamespace,
  onToggleNode,
  className,
}: {
  health: ClusterHealth;
  version: string;
  filters: PortalFilters;
  onToggleNamespace: (namespace: string) => void;
  onToggleNode: (node: string) => void;
  className?: string;
}) {
  return (
    <section aria-labelledby="cluster-health-heading" className={cn("flex flex-col gap-2", className)}>
      <div className="flex flex-wrap items-center gap-2">
        <h1 id="cluster-health-heading" className="text-xl font-semibold tracking-tight text-balance text-foreground">
          Cluster health
        </h1>
        <Badge variant="muted" className="font-mono">
          pagerless-server {version}
        </Badge>
        <span className="ml-auto flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
          <span className="inline-flex items-center gap-1">
            <Layers className="size-3" aria-hidden="true" /> {health.pages} Pages
          </span>
          <span className="inline-flex items-center gap-1">
            <Server className="size-3" aria-hidden="true" /> {health.namespaces.length} namespaces
          </span>
          <span className="inline-flex items-center gap-1">
            <Cpu className="size-3" aria-hidden="true" /> {health.nodes.length}{" "}
            {health.nodes.length === 1 ? "node Page" : "node Pages"}
          </span>
          <span className="inline-flex items-center gap-1 text-primary">
            <GitPullRequest className="size-3" aria-hidden="true" /> {health.merged} merged
          </span>
        </span>
      </div>

      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 xl:grid-cols-4">
        {health.namespaces.map((namespace) => (
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
              <span className="block text-xs text-muted-foreground">
                {health.nodes.length === 0
                  ? "no Node Page yet"
                  : `${health.nodes.length} with a Page`}
              </span>
            </span>
            <HealthPill
              health={healthOf(
                health.nodes.some((node) => node.severity === "critical")
                  ? "critical"
                  : health.nodes.some((node) => node.severity === "warning")
                    ? "warning"
                    : "info",
              )}
            />
          </div>
          <ul className="flex flex-1 flex-col gap-0.5 border-t border-border-subtle p-2">
            {health.nodes.map((node) => (
              <NodeRow
                key={node.name}
                node={node}
                selected={filters.node === node.name}
                onToggle={() => {
                  onToggleNode(node.name);
                }}
              />
            ))}
            {health.nodes.length === 0 && (
              <li className="px-1 text-xs text-muted-foreground">
                A Node fault opens a Page with the node as its Target; it appears here.
              </li>
            )}
          </ul>
        </div>
      </div>
    </section>
  );
}
