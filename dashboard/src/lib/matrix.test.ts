import { describe, expect, it } from "vitest";

import { buildClusterHealth, severityCounts } from "./matrix";
import type { PageSummary } from "./types";

function page(overrides: Partial<PageSummary> & Pick<PageSummary, "name">): PageSummary {
  const base: PageSummary = {
    name: "",
    namespace: "pagerless",
    phase: "Proposed",
    severity: "warning",
    createdAt: "2026-11-18T12:00:00Z",
    target: { kind: "Deployment", namespace: "shop", name: "checkout" },
    trigger: { source: "Event" },
    source: "live",
  };
  return { ...base, ...overrides };
}

const checkout = page({ name: "checkout-1", severity: "critical" });
const payments = page({
  name: "payments-1",
  severity: "info",
  phase: "Recovered",
  target: { kind: "Deployment", namespace: "payments", name: "payments-api" },
  pullRequest: { number: 12, url: "https://example.test/12", state: "Merged" },
});
const node = page({
  name: "node-1",
  severity: "warning",
  target: { kind: "Node", namespace: "", name: "ip-10-0-1-17" },
});

describe("cluster matrix", () => {
  it("groups namespaced Pages into one tile per namespace", () => {
    const health = buildClusterHealth([checkout, payments, node]);

    expect(health.namespaces.map((namespace) => namespace.name)).toEqual(["payments", "shop"]);
    expect(health.namespaces[1]?.counts).toEqual({ critical: 1, warning: 0, info: 0 });
    expect(health.namespaces[1]?.severity).toBe("critical");
    expect(health.namespaces[1]?.workloads.map((workload) => workload.name)).toEqual(["checkout"]);
  });

  it("lists Node Pages as node tiles and never as namespaces", () => {
    const health = buildClusterHealth([checkout, payments, node]);

    expect(health.nodes.map((entry) => entry.name)).toEqual(["ip-10-0-1-17"]);
    expect(health.namespaces.some((entry) => entry.name === "")).toBe(false);
  });

  it("counts Pages, Proposals awaiting a decision, and merged pull requests", () => {
    const health = buildClusterHealth([checkout, payments, node]);

    expect(health.pages).toBe(3);
    expect(health.proposed).toBe(2);
    expect(health.merged).toBe(1);
  });

  it("counts severity across the inbox", () => {
    expect(severityCounts([checkout, payments, node])).toEqual({ critical: 1, warning: 1, info: 1 });
  });
});
