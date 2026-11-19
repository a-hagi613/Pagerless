import type { Phase } from "@/lib/api";

import { Badge, type BadgeVariant } from "./ui/badge";

const variants: Record<Phase, BadgeVariant> = {
  Pending: "muted",
  Investigating: "info",
  Proposed: "info",
  Approved: "info",
  PullRequestOpen: "info",
  Verifying: "info",
  Recovered: "success",
  Diagnosed: "success",
  NotRecovered: "danger",
  Invalid: "danger",
  Failed: "danger",
  Superseded: "muted",
  Rejected: "warning",
  Expired: "warning",
};

export function PhaseBadge({ phase }: { phase: Phase }) {
  return <Badge variant={variants[phase]}>{phase}</Badge>;
}
