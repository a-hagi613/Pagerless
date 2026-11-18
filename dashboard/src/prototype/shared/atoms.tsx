import { Boxes, CircleCheck, CircleExclamationPoint, Cpu, Database, TriangleExclamationPoint } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

import { ageLabel, severityLabel, statusLabel } from "../format";
import type { Page, PageStatus, ResourceKind, Severity } from "../types";

const severityDot: Record<Severity, string> = {
  critical: "bg-destructive",
  warning: "bg-warning",
  info: "bg-muted-foreground",
};

export function SeverityDot({ severity, className }: { severity: Severity; className?: string }) {
  return (
    <span
      className={cn("size-2 shrink-0 rounded-full", severityDot[severity], className)}
      title={severityLabel(severity)}
      aria-label={severityLabel(severity)}
    />
  );
}

const statusVariant: Record<PageStatus, "success" | "warning" | "danger" | "muted" | "mint"> = {
  pending: "muted",
  investigating: "warning",
  ready: "success",
  "pr-open": "mint",
  recovered: "muted",
  dismissed: "muted",
};

export function PageStatusPill({ status }: { status: PageStatus }) {
  return (
    <Badge variant={statusVariant[status]}>
      {status === "ready" && <CircleCheck className="size-3" aria-hidden="true" />}
      {status === "pr-open" && <CircleExclamationPoint className="size-3" aria-hidden="true" />}
      {status === "recovered" && <CircleCheck className="size-3" aria-hidden="true" />}
      {statusLabel(status)}
    </Badge>
  );
}

export function HealthPill({ status }: { status: "healthy" | "degraded" | "critical" }) {
  if (status === "critical") {
    return (
      <Badge variant="danger">
        <CircleExclamationPoint className="size-3" aria-hidden="true" /> critical
      </Badge>
    );
  }
  if (status === "degraded") {
    return (
      <Badge variant="warning">
        <TriangleExclamationPoint className="size-3" aria-hidden="true" /> degraded
      </Badge>
    );
  }
  return (
    <Badge variant="success">
      <CircleCheck className="size-3" aria-hidden="true" /> healthy
    </Badge>
  );
}

const kindIcon: Record<ResourceKind, typeof Boxes> = {
  Deployment: Boxes,
  StatefulSet: Database,
  Pod: Boxes,
  Job: Cpu,
  CronJob: Cpu,
  PVC: Database,
  Node: Cpu,
};

export function WorkloadRef({ page, className }: { page: Page; className?: string }) {
  const Icon = kindIcon[page.kind];
  return (
    <span className={cn("inline-flex min-w-0 items-center gap-1.5 font-mono text-[12px] text-muted-foreground", className)}>
      <Icon className="size-3 shrink-0 text-muted-foreground" aria-hidden="true" />
      <span className="truncate">
        {page.namespace}/{page.workload}
      </span>
    </span>
  );
}

export function NamespaceChip({ namespace, className }: { namespace: string; className?: string }) {
  return (
    <span className={cn("rounded border border-border-subtle bg-muted px-1.5 py-0.5 font-mono text-[12px] text-muted-foreground", className)}>
      ns/{namespace}
    </span>
  );
}

export function AgeText({ minutes }: { minutes: number }) {
  return <span className="font-mono text-[12px] text-muted-foreground">{ageLabel(minutes)}</span>;
}

export function ConfidenceBar({ value, className }: { value: number; className?: string }) {
  return (
    <span className={cn("flex items-center gap-2", className)} title={`${String(value)}% agent confidence`}>
      <span className="h-1 w-16 overflow-hidden rounded-full bg-border-subtle">
        <span className="block h-full rounded-full bg-primary" style={{ width: `${String(value)}%` }} />
      </span>
      <span className="font-mono text-[12px] text-muted-foreground tabular-nums">{value}%</span>
    </span>
  );
}
