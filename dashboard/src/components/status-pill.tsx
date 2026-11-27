import {
  CircleCheck,
  CircleDashed,
  CircleX,
  Clock,
  LoaderCircle,
  TriangleAlert,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { statusLabel, statusTone, toolStatusLabel, toolStatusTone, type StatusTone } from "@/lib/format";
import type { PageStatus } from "@/lib/contract";
import { cn } from "@/lib/utils";

const icons: Record<StatusTone, typeof CircleCheck> = {
  success: CircleCheck,
  warning: TriangleAlert,
  danger: CircleX,
  info: LoaderCircle,
  muted: CircleDashed,
};

const variants: Record<StatusTone, "success" | "warning" | "danger" | "info" | "muted"> = {
  success: "success",
  warning: "warning",
  danger: "danger",
  info: "info",
  muted: "muted",
};

// Colour never carries the state alone: every pill pairs its tone with an icon and a word.
export function StatusPill({ status, className }: { status: PageStatus | string; className?: string }) {
  const tone = statusTone(status);
  const Icon = icons[tone];
  return (
    <Badge variant={variants[tone]} className={className} data-status={status}>
      <Icon className={cn("size-3", status === "running" && "motion-safe:animate-spin")} aria-hidden="true" />
      {statusLabel(status)}
    </Badge>
  );
}

export function OutcomePill({ status }: { status: string }) {
  const tone = toolStatusTone(status);
  const Icon = status === "running" ? Clock : icons[tone];
  return (
    <Badge variant={variants[tone]} data-outcome={status}>
      <Icon className={cn("size-3", status === "running" && "motion-safe:animate-spin")} aria-hidden="true" />
      {toolStatusLabel(status)}
    </Badge>
  );
}
