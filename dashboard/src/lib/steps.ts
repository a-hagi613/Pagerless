import type { Step } from "./types";

// stepKey identifies a step across the snapshot and the live stream, so a step is never shown twice. The
// API's Step carries no identifier, so the identity is its content.
export function stepKey(step: Step): string {
  return [step.time, step.kind, step.tool ?? "", step.summary].join("|");
}

// mergeSteps appends the steps the stream has not shown yet, preserving the order the server recorded.
export function mergeSteps(existing: readonly Step[], incoming: readonly Step[]): Step[] {
  const seen = new Set(existing.map((step) => stepKey(step)));
  return [...existing, ...incoming.filter((step) => !seen.has(stepKey(step)))];
}
