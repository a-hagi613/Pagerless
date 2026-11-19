import type { PageSummary, Phase, Severity, Subject } from "./types";

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

export function timestamp(iso: string): string {
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

export function severityLabel(severity: Severity): string {
  if (severity === "critical") {
    return "Critical";
  }
  if (severity === "warning") {
    return "Warning";
  }
  return "Info";
}

// The subject, in the order an operator says it: kind, namespace, name.
export function subjectLabel(subject: Subject): string {
  const kind = subject.kind.toLowerCase();
  if (subject.kind === "Node" || subject.namespace === undefined || subject.namespace === "") {
    return `${kind}/${subject.name}`;
  }
  return `${kind}/${subject.namespace}/${subject.name}`;
}

export function shortHash(hash: string | undefined): string {
  if (hash === undefined || hash === "") {
    return "none";
  }
  const digest = hash.startsWith("sha256:") ? hash.slice("sha256:".length) : hash;
  return digest.slice(0, 8);
}

// The card headline. The server may supply a summary; otherwise the subject is the headline.
export function pageTitle(page: PageSummary): string {
  if (page.title !== undefined && page.title !== "") {
    return page.title;
  }
  return subjectLabel(page.subject);
}

export function pageSignal(page: PageSummary): string {
  if (page.signal !== undefined && page.signal !== "") {
    return page.signal;
  }
  const triggers = page.triggerCount === 1 ? "1 trigger" : `${String(page.triggerCount)} triggers`;
  return `${subjectLabel(page.subject)} · ${triggers}`;
}
