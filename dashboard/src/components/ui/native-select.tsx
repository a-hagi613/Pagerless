import type { ComponentProps } from "react";

import { cn } from "@/lib/utils";

// A native select, deliberately: it keeps keyboard and screen-reader behaviour the platform already
// gets right, and the dashboard keeps the platform keyboard model for its hint fields.
function Select({ className, children, ...props }: ComponentProps<"select">) {
  return (
    <select
      data-slot="select"
      className={cn(
        "h-9 w-full min-w-0 rounded-md border border-input bg-card px-2 text-sm text-foreground transition-colors hover:border-border-strong focus-visible:border-ring disabled:cursor-not-allowed disabled:opacity-70",
        className,
      )}
      {...props}
    >
      {children}
    </select>
  );
}

export { Select };
