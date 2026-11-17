import { useEffect, useState } from "react";

// useNow re-renders a component on an interval, so relative ages stay current without a page reload.
export function useNow(intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => {
      setNow(Date.now());
    }, intervalMs);
    return () => {
      window.clearInterval(timer);
    };
  }, [intervalMs]);
  return now;
}

// formatAge renders a compact age in the style of kubectl and Argo CD.
export function formatAge(now: number, at: string): string {
  const timestamp = Date.parse(at);
  if (Number.isNaN(timestamp)) {
    return "unknown";
  }
  const seconds = Math.max(0, Math.floor((now - timestamp) / 1000));
  if (seconds < 60) {
    return `${String(seconds)}s`;
  }
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) {
    return `${String(minutes)}m`;
  }
  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    return `${String(hours)}h`;
  }
  const days = Math.floor(hours / 24);
  if (days < 7) {
    return `${String(days)}d`;
  }
  return `${String(Math.floor(days / 7))}w`;
}

// formatTime renders an instant for a tooltip or a timeline.
export function formatTime(at: string | undefined): string {
  if (at === undefined) {
    return "";
  }
  const timestamp = Date.parse(at);
  if (Number.isNaN(timestamp)) {
    return at;
  }
  return new Date(timestamp).toLocaleString(undefined, {
    dateStyle: "medium",
    timeStyle: "medium",
  });
}
