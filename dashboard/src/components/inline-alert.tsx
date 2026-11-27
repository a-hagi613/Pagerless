import { CircleAlert, Info, TriangleAlert } from "lucide-react";
import type { ReactNode } from "react";

import { cn } from "@/lib/utils";

export type AlertTone = "error" | "warning" | "info";

const tones: Record<AlertTone, { box: string; Icon: typeof Info }> = {
  error: { box: "border-destructive/40 bg-danger-surface text-destructive", Icon: CircleAlert },
  warning: { box: "border-warning/40 bg-warning-surface text-warning", Icon: TriangleAlert },
  info: { box: "border-info/40 bg-info-surface text-info", Icon: Info },
};

// A short, announced message for a refused request or an incomplete read. Errors use role=alert so a
// screen reader hears the failure once; informational notes are polite status text.
export function InlineAlert({
  tone = "error",
  title,
  children,
  action,
  className,
}: {
  tone?: AlertTone;
  title?: string;
  children?: ReactNode;
  action?: ReactNode;
  className?: string;
}) {
  const { box, Icon } = tones[tone];
  return (
    <div
      role={tone === "error" ? "alert" : "status"}
      className={cn("flex flex-wrap items-start gap-2 rounded-md border px-3 py-2 text-sm", box, className)}
    >
      <Icon className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
      <div className="min-w-0 flex-1">
        {title !== undefined && <p className="font-semibold">{title}</p>}
        {children !== undefined && <div className="text-foreground/90">{children}</div>}
      </div>
      {action !== undefined && <div className="shrink-0">{action}</div>}
    </div>
  );
}
