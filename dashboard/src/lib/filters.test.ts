import { describe, expect, it } from "vitest";

import {
  NO_FILTERS,
  filterPages,
  isFiltered,
  matchesFilters,
  scopeLabel,
  sortPages,
  toggleNamespace,
  toggleNode,
  toggleSeverity,
} from "./filters";
import type { PageSummary } from "./types";

function page(overrides: Partial<PageSummary> & Pick<PageSummary, "name">): PageSummary {
  const base: PageSummary = {
    name: "",
    phase: "Proposed",
    severity: "warning",
    createdAt: "2026-11-18T12:00:00Z",
    triggerCount: 1,
    subject: {
      apiVersion: "apps/v1",
      kind: "Deployment",
      namespace: "shop",
      name: "checkout",
      uid: "uid-checkout",
      resolution: "direct",
    },
  };
  return { ...base, ...overrides };
}

const critical = page({
  name: "checkout-7d9f",
  severity: "critical",
  createdAt: "2026-11-18T12:05:00Z",
  subject: {
    apiVersion: "apps/v1",
    kind: "Deployment",
    namespace: "shop",
    name: "checkout",
    uid: "uid-checkout",
    node: "ip-10-0-1-17",
    resolution: "direct",
  },
});
const warning = page({
  name: "payments-2xkq",
  severity: "warning",
  createdAt: "2026-11-18T12:02:00Z",
  subject: {
    apiVersion: "apps/v1",
    kind: "Deployment",
    namespace: "payments",
    name: "payments-api",
    uid: "uid-payments",
    node: "ip-10-0-2-21",
    resolution: "direct",
  },
});
const info = page({
  name: "ip-10-0-1-17",
  severity: "info",
  createdAt: "2026-11-18T11:58:00Z",
  subject: {
    apiVersion: "v1",
    kind: "Node",
    name: "ip-10-0-1-17",
    uid: "uid-node",
    resolution: "cluster",
  },
});

describe("inbox ordering", () => {
  it("orders by severity, then newest inside a severity", () => {
    const olderCritical = { ...critical, name: "older-critical", createdAt: "2026-11-18T11:00:00Z" };

    expect(sortPages([info, warning, olderCritical, critical]).map((entry) => entry.name)).toEqual([
      "checkout-7d9f",
      "older-critical",
      "payments-2xkq",
      "ip-10-0-1-17",
    ]);
  });
});

describe("cluster-shape filters", () => {
  it("shows every Page when nothing is filtered", () => {
    expect(filterPages([critical, warning, info], NO_FILTERS)).toHaveLength(3);
    expect(isFiltered(NO_FILTERS)).toBe(false);
  });

  it("narrows to one namespace", () => {
    const filters = toggleNamespace(NO_FILTERS, "shop");

    expect(filterPages([critical, warning, info], filters).map((entry) => entry.name)).toEqual(["checkout-7d9f"]);
    expect(isFiltered(filters)).toBe(true);
  });

  it("toggles the same namespace tile back to the whole cluster", () => {
    const once = toggleNamespace(NO_FILTERS, "shop");
    const twice = toggleNamespace(once, "shop");

    expect(twice).toEqual(NO_FILTERS);
  });

  it("narrows to a node, matching both a Node Page and the workflows scheduled on it", () => {
    const filters = toggleNode(NO_FILTERS, "ip-10-0-1-17");
    const matches = filterPages([critical, warning, info], filters);

    expect(matches.map((entry) => entry.name)).toEqual(["checkout-7d9f", "ip-10-0-1-17"]);
  });

  it("narrows to one severity", () => {
    const filters = toggleSeverity(NO_FILTERS, "warning");

    expect(filterPages([critical, warning, info], filters).map((entry) => entry.name)).toEqual(["payments-2xkq"]);
  });

  it("combines namespace, node, and severity", () => {
    const filters = { namespace: "shop", node: "ip-10-0-1-17", severity: "critical" as const };

    expect(matchesFilters(critical, filters)).toBe(true);
    expect(matchesFilters(warning, filters)).toBe(false);
    expect(matchesFilters(info, filters)).toBe(false);
  });

  it("describes the active scope in words", () => {
    expect(scopeLabel(NO_FILTERS)).toBe("whole cluster");
    expect(scopeLabel({ namespace: "shop", node: null, severity: null })).toBe("namespace shop");
    expect(scopeLabel({ namespace: "shop", node: "ip-10-0-1-17", severity: "critical" })).toBe(
      "namespace shop · node ip-10-0-1-17 · critical severity",
    );
  });
});
