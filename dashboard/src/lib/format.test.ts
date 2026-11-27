import { describe, expect, it } from "vitest";

import {
  citationLabel,
  conversationTitle,
  elapsedLabel,
  safeExternalURL,
  shortId,
  statusLabel,
  statusTone,
  toolStatusLabel,
  toolStatusTone,
} from "./format";

describe("status language", () => {
  it("labels every turn status", () => {
    expect(statusLabel("running")).toBe("Running");
    expect(statusLabel("completed")).toBe("Complete");
    expect(statusLabel("stopped")).toBe("Stopped");
    expect(statusLabel("failed")).toBe("Failed");
    expect(statusLabel("interrupted")).toBe("Interrupted");
  });

  it("pairs every status with a non-colour tone", () => {
    expect(statusTone("completed")).toBe("success");
    expect(statusTone("failed")).toBe("danger");
    expect(statusTone("interrupted")).toBe("warning");
  });

  it("humanizes tool outcomes without hiding the contract value", () => {
    expect(toolStatusLabel("timed_out")).toBe("timed out");
    expect(toolStatusLabel("budget_exhausted")).toBe("budget exhausted");
    expect(toolStatusTone("truncated")).toBe("warning");
    expect(toolStatusTone("denied")).toBe("danger");
  });
});

describe("evidence and timing labels", () => {
  it("keeps the JSON pointer beside the citation call id", () => {
    expect(citationLabel({ callId: "c4", pointer: "/spec/replicas" })).toBe("c4 /spec/replicas");
    expect(citationLabel({ callId: "c4", jsonPointer: "/status" })).toBe("c4 /status");
  });

  it("formats sub-second and second durations", () => {
    expect(elapsedLabel(12)).toBe("12 ms");
    expect(elapsedLabel(1_240)).toBe("1.2 s");
    expect(elapsedLabel(12_400)).toBe("12 s");
    expect(elapsedLabel(undefined)).toBe("");
  });

  it("shortens only long identifiers", () => {
    expect(shortId("c4")).toBe("c4");
    expect(shortId("4f3c2b1a9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b")).toBe("4f3c2b1a");
  });

  it("only turns absolute HTTP(S) server links into anchors", () => {
    expect(safeExternalURL("https://github.com/example/shop/pull/42")).toBe(
      "https://github.com/example/shop/pull/42",
    );
    expect(safeExternalURL("http://example.test/pr/1")).toBe("http://example.test/pr/1");
    expect(safeExternalURL("javascript:alert(1)")).toBeUndefined();
    expect(safeExternalURL("/relative/pull/1")).toBeUndefined();
    expect(safeExternalURL(undefined)).toBeUndefined();
  });
});

describe("conversation copy", () => {
  it("builds a one-line title and truncates it", () => {
    expect(conversationTitle("  Why is\n checkout slow? ")).toBe("Why is checkout slow?");
    expect(conversationTitle("x".repeat(100))).toHaveLength(78);
  });
});
