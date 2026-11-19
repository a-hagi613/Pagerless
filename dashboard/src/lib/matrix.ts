import { pageSeverity } from "./format";
import type { PageSummary, Severity } from "./types";

// The cluster matrix is derived from the Pages the server returns. There is no separate cluster endpoint:
// every tile is a scope, and the numbers on it are the Pages inside that scope. A namespace with no Page
// has nothing to show and no tile, which is the honest shape of the data the Portal is allowed to read.

export interface WorkloadTile {
  name: string;
  kind: string;
  phase: PageSummary["phase"];
  severity: Severity;
  page: string;
}

export interface NamespaceHealth {
  name: string;
  severity: Severity;
  pages: PageSummary[];
  counts: Record<Severity, number>;
  workloads: WorkloadTile[];
}

export interface NodeHealth {
  name: string;
  severity: Severity;
  pages: PageSummary[];
}

export interface ClusterHealth {
  namespaces: NamespaceHealth[];
  nodes: NodeHealth[];
  pages: number;
  proposed: number;
  merged: number;
}

const rank: Record<Severity, number> = { critical: 2, warning: 1, info: 0 };

function worst(severities: readonly Severity[]): Severity {
  let current: Severity = "info";
  for (const severity of severities) {
    if (rank[severity] > rank[current]) {
      current = severity;
    }
  }
  return current;
}

function workload(page: PageSummary): WorkloadTile {
  return {
    name: page.target.name,
    kind: page.target.kind,
    phase: page.phase,
    severity: pageSeverity(page),
    page: page.name,
  };
}

export function buildClusterHealth(pages: readonly PageSummary[]): ClusterHealth {
  const namespaces = new Map<string, PageSummary[]>();
  const nodes = new Map<string, PageSummary[]>();
  let proposed = 0;
  let merged = 0;

  for (const page of pages) {
    if (page.phase === "Proposed") {
      proposed += 1;
    }
    if (page.pullRequest?.state === "Merged") {
      merged += 1;
    }
    if (page.target.kind === "Node") {
      nodes.set(page.target.name, [...(nodes.get(page.target.name) ?? []), page]);
      continue;
    }
    if (page.target.namespace === "") {
      continue;
    }
    namespaces.set(page.target.namespace, [...(namespaces.get(page.target.namespace) ?? []), page]);
  }

  return {
    namespaces: [...namespaces.entries()]
      .map(([name, scoped]) => {
        const counts: Record<Severity, number> = { critical: 0, warning: 0, info: 0 };
        for (const page of scoped) {
          counts[pageSeverity(page)] += 1;
        }
        return {
          name,
          pages: scoped,
          counts,
          severity: worst(scoped.map(pageSeverity)),
          workloads: scoped.map(workload).sort((left, right) => left.name.localeCompare(right.name)),
        };
      })
      .sort((left, right) => left.name.localeCompare(right.name)),
    nodes: [...nodes.entries()]
      .map(([name, scoped]) => ({
        name,
        pages: scoped,
        severity: worst(scoped.map(pageSeverity)),
      }))
      .sort((left, right) => left.name.localeCompare(right.name)),
    pages: pages.length,
    proposed,
    merged,
  };
}

export function severityCounts(pages: readonly PageSummary[]): Record<Severity, number> {
  const counts: Record<Severity, number> = { critical: 0, warning: 0, info: 0 };
  for (const page of pages) {
    counts[pageSeverity(page)] += 1;
  }
  return counts;
}
