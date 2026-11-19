import type { PageSummary, Phase, Severity, TargetReference } from "./types";

const minute = 60_000;
const hour = 60 * minute;
const day = 24 * hour;

export function ageLabel(iso: string, now: number = Date.now()): string {
  const then = Date.parse(iso);
  if (Number.isNaN(then)) {
    return "unknown";
  }
  const elapsed = Math.max(0, now - then);
  if (elapsed < minute) {
    return `${String(Math.max(1, Math.floor(elapsed / 1000)))}s`;
  }
  if (elapsed < hour) {
    return `${String(Math.floor(elapsed / minute))}m`;
  }
  if (elapsed < day) {
    return `${String(Math.floor(elapsed / hour))}h`;
  }
  return `${String(Math.floor(elapsed / day))}d`;
}

export function timestamp(iso: string | undefined): string {
  if (iso === undefined || iso === "") {
    return "—";
  }
  const parsed = Date.parse(iso);
  if (Number.isNaN(parsed)) {
    return iso;
  }
  return new Date(parsed).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function timeOfDay(iso: string): string {
  const parsed = Date.parse(iso);
  if (Number.isNaN(parsed)) {
    return iso;
  }
  return new Date(parsed).toLocaleTimeString(undefined, { hour12: false });
}

const phaseLabels: Record<Phase, string> = {
  Pending: "Pending",
  Investigating: "Investigating",
  Proposed: "Proposed",
  Approved: "Approved",
  PullRequestOpen: "PR open",
  Verifying: "Verifying",
  Recovered: "Recovered",
  NotRecovered: "Not recovered",
  Diagnosed: "Diagnosed",
  Rejected: "Rejected",
  Expired: "Expired",
  Invalid: "Invalid",
  Superseded: "Superseded",
  Failed: "Failed",
};

export function phaseLabel(phase: Phase): string {
  return phaseLabels[phase];
}

const terminalPhases: ReadonlySet<Phase> = new Set<Phase>([
  "Recovered",
  "NotRecovered",
  "Diagnosed",
  "Rejected",
  "Expired",
  "Invalid",
  "Superseded",
  "Failed",
]);

export function isTerminal(phase: Phase): boolean {
  return terminalPhases.has(phase);
}

export function isLive(phase: Phase): boolean {
  return phase === "Investigating" || phase === "Verifying";
}

// A Page records its own Severity. A Page that predates the field, or one that has not been ranked yet,
// falls back to a Phase-derived rank so the inbox still orders it deterministically.
export function pageSeverity(page: Pick<PageSummary, "phase" | "severity">): Severity {
  if (page.severity === "critical" || page.severity === "warning" || page.severity === "info") {
    return page.severity;
  }
  switch (page.phase) {
    case "Failed":
    case "NotRecovered":
    case "Invalid":
      return "critical";
    case "Pending":
    case "Investigating":
    case "Proposed":
    case "Approved":
    case "PullRequestOpen":
    case "Verifying":
      return "warning";
    default:
      return "info";
  }
}

export function severityLabel(severity: Severity): string {
  if (severity === "critical") {
    return "Critical";
  }
  if (severity === "warning") {
    return "Warning";
  }
  return "Info";
}

// The target, in the order an operator says it: kind, namespace, name. A Node is cluster-scoped.
export function targetLabel(target: TargetReference): string {
  const kind = target.kind.toLowerCase();
  if (target.kind === "Node" || target.namespace === "") {
    return `${kind}/${target.name}`;
  }
  return `${kind}/${target.namespace}/${target.name}`;
}

export function shortHash(hash: string | undefined): string {
  if (hash === undefined || hash === "") {
    return "none";
  }
  const digest = hash.startsWith("sha256:") ? hash.slice("sha256:".length) : hash;
  return digest.slice(0, 8);
}

// The card headline is the Target, because a Page is one investigation of one Target.
export function pageTitle(page: PageSummary): string {
  return page.target.name;
}

export function pageSignal(page: PageSummary): string {
  const scope =
    page.target.kind === "Node" || page.target.namespace === ""
      ? page.target.kind
      : `${page.target.kind} in ${page.target.namespace}`;
  const trigger = page.trigger.pod === undefined ? "" : ` · pod ${page.trigger.pod}`;
  return `${scope} · ${page.trigger.source}${trigger}`;
}

export function proposalTitle(action: string, path: string | undefined): string {
  if (action === "NoAction" || path === undefined || path === "") {
    return "No file change";
  }
  return path;
}
