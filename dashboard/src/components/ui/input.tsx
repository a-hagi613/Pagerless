import type { ComponentProps } from "react";

import { cn } from "@/lib/utils";

function Input({ className, type, ...props }: ComponentProps<"input">) {
  return (
    <input
      type={type}
      data-slot="input"
      className={cn(
        "h-9 w-full min-w-0 rounded-md border border-input bg-card px-2.5 py-1 text-sm text-foreground transition-colors placeholder:text-muted-foreground hover:border-border-strong focus-visible:border-ring disabled:cursor-not-allowed disabled:opacity-70 read-only:bg-muted",
        className,
      )}
      {...props}
    />
  );
}

function Textarea({ className, ...props }: ComponentProps<"textarea">) {
  return (
    <textarea
      data-slot="textarea"
      className={cn(
        "min-h-16 w-full resize-y rounded-md border border-input bg-card px-2.5 py-1.5 text-sm text-foreground transition-colors placeholder:text-muted-foreground hover:border-border-strong focus-visible:border-ring disabled:cursor-not-allowed disabled:opacity-70",
        className,
      )}
      {...props}
    />
  );
}

export { Input, Textarea };
