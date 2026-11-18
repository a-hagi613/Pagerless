import type { PageStatus, Severity } from "./types";

export function ageLabel(minutes: number): string {
  if (minutes < 1) {
    return "just now";
  }
  if (minutes < 60) {
    return `${String(minutes)}m ago`;
  }
  const hours = Math.floor(minutes / 60);
  return `${String(hours)}h ago`;
}

export function shortHash(hash: string): string {
  return hash.replace(/^sha256:/u, "").slice(0, 12);
}

export function severityLabel(severity: Severity): string {
  switch (severity) {
    case "critical":
      return "Sev 1";
    case "warning":
      return "Sev 2";
    case "info":
      return "Sev 3";
  }
}

export function statusLabel(status: PageStatus): string {
  switch (status) {
    case "pending":
      return "Pending diagnosis";
    case "investigating":
      return "Investigating";
    case "ready":
      return "Ready to create PR";
    case "pr-open":
      return "PR open";
    case "recovered":
      return "Recovered";
    case "dismissed":
      return "Dismissed";
  }
}
