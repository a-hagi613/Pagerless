import { describe, expect, it } from "vitest";

import {
  CLI_VERBS,
  PORTAL_ACTIONS,
  parityReport,
  thisPageCommands,
  verbsInGroup,
  type CliVerb,
  type PortalAction,
} from "./cli-parity";

describe("Portal and CLI parity", () => {
  it("pairs every CLI verb with exactly one Portal path and every Portal path with one verb", () => {
    const report = parityReport();

    expect(report.unpairedVerbs).toEqual([]);
    expect(report.unpairedActions).toEqual([]);
    expect(report.duplicatedVerbs).toEqual([]);
    expect(report.ok).toBe(true);
  });

  it("covers the whole documented kubectl page surface", () => {
    // The v2 command surface: <TARGET>, list, show, diff, approve, dismiss, config {set,get,test,unset},
    // version. A new verb must arrive with a Portal path in the same change.
    expect(CLI_VERBS.map((verb) => verb.id).sort()).toEqual(
      [
        "approve",
        "config-get",
        "config-set",
        "config-test",
        "config-unset",
        "diff",
        "dismiss",
        "list",
        "page",
        "show",
        "version",
      ].sort(),
    );
    expect(PORTAL_ACTIONS).toHaveLength(CLI_VERBS.length);
  });

  it("fails when a CLI verb has no Portal path", () => {
    const verbs: CliVerb[] = CLI_VERBS.filter((verb) => verb.id !== "dismiss");
    const report = parityReport(verbs, PORTAL_ACTIONS);

    expect(report.ok).toBe(false);
    expect(report.unpairedActions).toEqual(["dismiss-page"]);
  });

  it("fails when a Portal action has no CLI verb", () => {
    const actions: PortalAction[] = [
      ...PORTAL_ACTIONS,
      { id: "retry-page", label: "Retry a Page", surface: "card", cliVerb: "watch" as CliVerb["id"] },
    ];
    const report = parityReport(CLI_VERBS, actions);

    expect(report.ok).toBe(false);
    expect(report.unpairedActions).toEqual(["retry-page"]);
  });

  it("fails when two Portal paths claim one verb", () => {
    const actions: PortalAction[] = [
      ...PORTAL_ACTIONS,
      { id: "second-approve", label: "Approve again", surface: "header", cliVerb: "approve" },
    ];
    const report = parityReport(CLI_VERBS, actions);

    expect(report.ok).toBe(false);
    expect(report.duplicatedVerbs).toEqual(["approve"]);
  });

  it("groups every verb into exactly one drawer section", () => {
    const grouped = ["Pages", "Decisions", "Setup"].flatMap((group) =>
      verbsInGroup(group as "Pages" | "Decisions" | "Setup").map((verb) => verb.id),
    );

    expect(grouped.sort()).toEqual(CLI_VERBS.map((verb) => verb.id).sort());
  });
});

describe("This Page CLI tab", () => {
  it("binds show, diff, approve, and dismiss to the expanded Page", () => {
    const commands = thisPageCommands({ name: "checkout-7d9f", namespace: "shop" });

    expect(commands.map((entry) => entry.command)).toEqual([
      "kubectl page show checkout-7d9f -n shop --steps",
      "kubectl page diff checkout-7d9f",
      "kubectl page approve checkout-7d9f --create-pr",
      'kubectl page dismiss checkout-7d9f --reason "<text>"',
    ]);
  });

  it("omits the namespace flag for a cluster-scoped Node Page", () => {
    const [show] = thisPageCommands({ name: "ip-10-0-1-17" });

    expect(show?.command).toBe("kubectl page show ip-10-0-1-17 --steps");
  });
});
