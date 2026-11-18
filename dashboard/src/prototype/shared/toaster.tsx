import { CircleCheck, CircleExclamationPoint, Info, X } from "lucide-react";

import { cn } from "@/lib/utils";

import { usePrototype } from "../store-context";
import type { Toast } from "../types";

const toneIcon = {
  success: CircleCheck,
  info: Info,
  warning: CircleExclamationPoint,
} as const;

const toneClass: Record<Toast["tone"], string> = {
  success: "text-primary",
  info: "text-mint",
  warning: "text-warning",
};

export function PrototypeToaster() {
  const { toasts, dismissToast } = usePrototype();

  return (
    <div
      aria-live="polite"
      aria-atomic="false"
      className="pointer-events-none fixed right-3 bottom-24 z-[70] flex w-[min(92vw,360px)] flex-col gap-2 sm:right-4 sm:bottom-24"
    >
      {toasts.map((toast) => {
        const Icon = toneIcon[toast.tone];
        return (
          <div
            key={toast.id}
            className="pointer-events-auto flex items-start gap-2 rounded-lg border border-border bg-popover/95 px-3 py-2 shadow-xl backdrop-blur"
          >
            <Icon className={cn("mt-0.5 size-4 shrink-0", toneClass[toast.tone])} aria-hidden="true" />
            <div className="min-w-0 flex-1">
              <p className="text-xs font-medium text-foreground">{toast.title}</p>
              <p className="mt-0.5 text-[12px] leading-snug text-muted-foreground">{toast.detail}</p>
            </div>
            <button
              type="button"
              onClick={() => {
                dismissToast(toast.id);
              }}
              className="rounded p-0.5 text-muted-foreground transition-colors hover:text-foreground"
              aria-label="Dismiss notification"
            >
              <X className="size-3.5" />
            </button>
          </div>
        );
      })}
    </div>
  );
}
