import { pageSeverity } from "./format";
import type { PageSummary, Severity } from "./types";

// Cluster shape is the navigation control: a namespace tile or a node tile narrows the inbox, and the
// brand header clears every filter. The rules live here so they can be tested without a browser.

export interface PortalFilters {
  namespace: string | null;
  node: string | null;
  severity: Severity | null;
}

export const NO_FILTERS: PortalFilters = { namespace: null, node: null, severity: null };

const severityOrder: Record<Severity, number> = { critical: 0, warning: 1, info: 2 };

export function isFiltered(filters: PortalFilters): boolean {
  return filters.namespace !== null || filters.node !== null || filters.severity !== null;
}

// A Page belongs to a node when the node is the Target of that Page. A workload Page does not carry its
// node in the API, so the node filter matches the Node Pages themselves and the workloads a Node fault
// names are visible on the tile.
export function matchesFilters(page: PageSummary, filters: PortalFilters): boolean {
  if (filters.namespace !== null && page.target.namespace !== filters.namespace) {
    return false;
  }
  if (filters.node !== null) {
    if (page.target.kind !== "Node" || page.target.name !== filters.node) {
      return false;
    }
  }
  if (filters.severity !== null && pageSeverity(page) !== filters.severity) {
    return false;
  }
  return true;
}

// Severity first, then newest inside a severity, then name so the order is stable across polls.
export function sortPages(pages: readonly PageSummary[]): PageSummary[] {
  return [...pages].sort((left, right) => {
    const bySeverity = severityOrder[pageSeverity(left)] - severityOrder[pageSeverity(right)];
    if (bySeverity !== 0) {
      return bySeverity;
    }
    const byAge = Date.parse(right.createdAt) - Date.parse(left.createdAt);
    if (byAge !== 0 && !Number.isNaN(byAge)) {
      return byAge;
    }
    return left.name.localeCompare(right.name);
  });
}

export function filterPages(pages: readonly PageSummary[], filters: PortalFilters): PageSummary[] {
  return sortPages(pages.filter((page) => matchesFilters(page, filters)));
}

export function toggleNamespace(filters: PortalFilters, namespace: string): PortalFilters {
  return { ...filters, namespace: filters.namespace === namespace ? null : namespace };
}

export function toggleNode(filters: PortalFilters, node: string): PortalFilters {
  return { ...filters, node: filters.node === node ? null : node };
}

export function toggleSeverity(filters: PortalFilters, severity: Severity): PortalFilters {
  return { ...filters, severity: filters.severity === severity ? null : severity };
}

export function scopeLabel(filters: PortalFilters): string {
  const parts: string[] = [];
  if (filters.namespace !== null) {
    parts.push(`namespace ${filters.namespace}`);
  }
  if (filters.node !== null) {
    parts.push(`node ${filters.node}`);
  }
  if (filters.severity !== null) {
    parts.push(`${filters.severity} severity`);
  }
  if (parts.length === 0) {
    return "whole cluster";
  }
  return parts.join(" · ");
}
