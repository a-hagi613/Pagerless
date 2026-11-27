import type { Citation, PageStatus } from "./contract";

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

export function elapsedLabel(milliseconds: number | undefined): string {
  if (milliseconds === undefined || !Number.isFinite(milliseconds) || milliseconds < 0) {
    return "";
  }
  if (milliseconds < 1000) {
    return `${String(Math.round(milliseconds))} ms`;
  }
  return `${(milliseconds / 1000).toFixed(milliseconds < 10_000 ? 1 : 0)} s`;
}

const statusLabels: Record<PageStatus, string> = {
  running: "Running",
  completed: "Complete",
  stopped: "Stopped",
  failed: "Failed",
  interrupted: "Interrupted",
};

export function statusLabel(status: PageStatus | string): string {
  if (status in statusLabels) {
    return statusLabels[status as PageStatus];
  }
  return humanizeToken(status);
}

export function isTerminalStatus(status: PageStatus | string): boolean {
  return status === "completed" || status === "stopped" || status === "failed" || status === "interrupted";
}

export type StatusTone = "success" | "warning" | "danger" | "info" | "muted";

export function statusTone(status: PageStatus | string): StatusTone {
  switch (status) {
    case "running":
      return "info";
    case "completed":
      return "success";
    case "stopped":
      return "warning";
    case "interrupted":
      return "warning";
    case "failed":
      return "danger";
    default:
      return "muted";
  }
}

// Tool outcomes come from the contract: ok, error, denied, truncated, timed_out and budget_exhausted. A
// new outcome still reads as a word beside its colour rather than as an unlabelled dot.
export function toolStatusLabel(status: string): string {
  switch (status) {
    case "ok":
      return "ok";
    case "error":
      return "error";
    case "denied":
      return "denied";
    case "truncated":
      return "truncated";
    case "timed_out":
      return "timed out";
    case "budget_exhausted":
      return "budget exhausted";
    case "running":
      return "running";
    default:
      return humanizeToken(status);
  }
}

export function toolStatusTone(status: string): StatusTone {
  if (status === "ok" || status === "completed") {
    return "success";
  }
  if (status === "running") {
    return "info";
  }
  if (status === "truncated" || status === "budget_exhausted" || status === "timed_out") {
    return "warning";
  }
  return "danger";
}

export function humanizeToken(value: string): string {
  const words = value.replaceAll("_", " ").replaceAll("-", " ").trim();
  return words === "" ? "unknown" : words.charAt(0).toUpperCase() + words.slice(1);
}

export function shortId(value: string): string {
  return value.length <= 12 ? value : value.slice(0, 8);
}

// A server-supplied link is untrusted like any other field. Only an absolute HTTP(S) URL may become an
// anchor target; anything else is dropped rather than rendered as a clickable scheme.
export function safeExternalURL(value: string | undefined): string | undefined {
  if (value === undefined || value === "") {
    return undefined;
  }
  try {
    const url = new URL(value);
    if (url.protocol === "http:" || url.protocol === "https:") {
      return value;
    }
  } catch {
    // A relative or malformed value is not a publishable pull-request link.
  }
  return undefined;
}

export function citationLabel(citation: Citation): string {
  const pointer = citation.pointer ?? citation.jsonPointer;
  return pointer === undefined || pointer === "" ? citation.callId : `${citation.callId} ${pointer}`;
}

// A signal is a compact, human-readable summary for a conversation that has no title yet.
export function conversationTitle(message: string): string {
  const line = message.replaceAll(/\s+/g, " ").trim();
  if (line.length <= 80) {
    return line;
  }
  return `${line.slice(0, 77)}…`;
}
