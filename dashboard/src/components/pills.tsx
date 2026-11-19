import {
  Activity,
  CircleCheck,
  CircleExclamationPoint,
  CircleSlash,
  CircleX,
  Clock,
  GitPullRequest,
  Info,
  Layers,
  Radar,
  Stethoscope,
  TriangleExclamationPoint,
  type LucideIcon,
} from "lucide-react";

import { Badge, type BadgeVariant } from "@/components/ui/badge";
import { phaseLabel, severityLabel } from "@/lib/format";
import type { Phase, Severity } from "@/lib/types";
import { cn } from "@/lib/utils";

// Every pill pairs its colour with an icon and a word, so no state is carried by colour alone.

const severityStyle: Record<Severity, { variant: BadgeVariant; Icon: LucideIcon }> = {
  critical: { variant: "danger", Icon: CircleExclamationPoint },
  warning: { variant: "warning", Icon: TriangleExclamationPoint },
  info: { variant: "info", Icon: Info },
};

export function SeverityPill({ severity, className }: { severity: Severity; className?: string }) {
  const { variant, Icon } = severityStyle[severity];
  return (
    <Badge variant={variant} className={className}>
      <Icon className="size-3" aria-hidden="true" />
      {severityLabel(severity)}
    </Badge>
  );
}

const phaseStyle: Record<Phase, { variant: BadgeVariant; Icon: LucideIcon }> = {
  Pending: { variant: "muted", Icon: Clock },
  Investigating: { variant: "info", Icon: Radar },
  Proposed: { variant: "default", Icon: GitPullRequest },
  Approved: { variant: "mint", Icon: CircleCheck },
  PullRequestOpen: { variant: "mint", Icon: GitPullRequest },
  Verifying: { variant: "info", Icon: Activity },
  Recovered: { variant: "success", Icon: CircleCheck },
  NotRecovered: { variant: "danger", Icon: CircleX },
  Diagnosed: { variant: "muted", Icon: Stethoscope },
  Rejected: { variant: "muted", Icon: CircleSlash },
  Expired: { variant: "muted", Icon: Clock },
  Invalid: { variant: "danger", Icon: CircleX },
  Superseded: { variant: "muted", Icon: Layers },
  Failed: { variant: "danger", Icon: TriangleExclamationPoint },
};

export function PhasePill({ phase, className }: { phase: Phase; className?: string }) {
  const { variant, Icon } = phaseStyle[phase];
  return (
    <Badge variant={variant} className={className}>
      <Icon className="size-3" aria-hidden="true" />
      {phaseLabel(phase)}
    </Badge>
  );
}

export type Health = "healthy" | "degraded" | "critical";

const healthStyle: Record<Health, { variant: BadgeVariant; Icon: LucideIcon; label: string }> = {
  healthy: { variant: "success", Icon: CircleCheck, label: "Healthy" },
  degraded: { variant: "warning", Icon: TriangleExclamationPoint, label: "Degraded" },
  critical: { variant: "danger", Icon: CircleExclamationPoint, label: "Critical" },
};

export function HealthPill({ health, className }: { health: Health; className?: string }) {
  const { variant, Icon, label } = healthStyle[health];
  return (
    <Badge variant={variant} className={className}>
      <Icon className="size-3" aria-hidden="true" />
      {label}
    </Badge>
  );
}

// A static status label. There is no pulse and no glow: the trace itself growing is the live signal.
export function LiveLabel({ label, className }: { label: string; className?: string }) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 font-mono text-xs tracking-[0.14em] text-muted-foreground uppercase",
        className,
      )}
    >
      <span className="size-1.5 rounded-full bg-primary" aria-hidden="true" />
      {label}
    </span>
  );
}

export function NamespaceChip({ namespace, className }: { namespace?: string; className?: string }) {
  return (
    <span
      className={cn(
        "rounded border border-border bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground",
        className,
      )}
    >
      {namespace === undefined || namespace === "" ? "cluster" : `ns/${namespace}`}
    </span>
  );
}
