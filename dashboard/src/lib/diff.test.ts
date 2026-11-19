import { describe, expect, it } from "vitest";

import { diffCounts, parseUnifiedDiff } from "./diff";

const sample = [
  "diff --git a/apps/shop/deployment.yaml b/apps/shop/deployment.yaml",
  "index 1a2b3c4..5d6e7f8 100644",
  "--- a/apps/shop/deployment.yaml",
  "+++ b/apps/shop/deployment.yaml",
  "@@ -18,7 +18,7 @@ spec:",
  "     spec:",
  "       containers:",
  "         - name: checkout",
  "-          readinessProbe:",
  "+          readinessProbe:",
  "+            timeoutSeconds: 4",
  "             httpGet:",
  "               path: /healthz",
].join("\n");

describe("unified diff parsing", () => {
  it("separates file chrome from changed lines and numbers them", () => {
    const lines = parseUnifiedDiff(sample);

    expect(lines[0]).toEqual({
      kind: "meta",
      text: "diff --git a/apps/shop/deployment.yaml b/apps/shop/deployment.yaml",
    });
    expect(lines.filter((line) => line.kind === "meta").map((line) => line.text)).toEqual([
      "diff --git a/apps/shop/deployment.yaml b/apps/shop/deployment.yaml",
      "index 1a2b3c4..5d6e7f8 100644",
      "--- a/apps/shop/deployment.yaml",
      "+++ b/apps/shop/deployment.yaml",
      "@@ -18,7 +18,7 @@ spec:",
    ]);
    const removed = lines.find((line) => line.kind === "remove");
    const added = lines.filter((line) => line.kind === "add");
    expect(removed).toEqual({ kind: "remove", text: "          readinessProbe:", before: 21, after: undefined });
    expect(added.map((line) => line.text)).toEqual(["          readinessProbe:", "            timeoutSeconds: 4"]);
    expect(added[0]?.after).toBe(21);
    expect(added[1]?.after).toBe(22);
  });

  it("treats +++ and --- headers as chrome, not as a change", () => {
    const lines = parseUnifiedDiff("--- a/x.yaml\n+++ b/x.yaml\n@@ -1,1 +1,2 @@\n old\n+new\n");

    expect(lines.filter((line) => line.kind === "add")).toHaveLength(1);
    expect(lines.filter((line) => line.kind === "remove")).toHaveLength(0);
  });

  it("counts additions and removals independently of their order", () => {
    expect(diffCounts(parseUnifiedDiff(sample))).toEqual({ added: 2, removed: 1 });
  });
});
