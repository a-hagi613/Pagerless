import { CircleCheck, CircleExclamationPoint, Info } from "lucide-react";
import type { ReactNode } from "react";

import { cn } from "@/lib/utils";

type Tone = "info" | "success" | "error";

const toneStyle: Record<Tone, string> = {
  info: "border-info/40 bg-info-surface text-foreground",
  success: "border-primary/40 bg-primary-soft text-primary-soft-foreground",
  error: "border-destructive/40 bg-danger-surface text-destructive",
};

const toneIcon = {
  info: Info,
  success: CircleCheck,
  error: CircleExclamationPoint,
};

// Errors and results render inline, beside the control that caused them. Neither the Portal nor the
// settings page raises a dialog for a refusal.
export function Notice({
  tone = "info",
  children,
  className,
}: {
  tone?: Tone;
  children: ReactNode;
  className?: string;
}) {
  const Icon = toneIcon[tone];
  return (
    <p
      role={tone === "error" ? "alert" : "status"}
      className={cn("flex items-start gap-2 rounded-md border px-3 py-2 text-sm leading-relaxed", toneStyle[tone], className)}
    >
      <Icon className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
      <span className="min-w-0">{children}</span>
    </p>
  );
}
