import type { ComponentProps } from "react";
import { cva, type VariantProps } from "class-variance-authority";

import { cn } from "@/lib/utils";

// The prototype's status language: solid for live state, tinted for history.
const badgeVariants = cva(
  "inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 font-mono text-[12px] leading-4 font-medium whitespace-nowrap",
  {
    variants: {
      variant: {
        default: "border-primary/40 bg-primary/15 text-primary",
        secondary: "border-border bg-secondary text-secondary-foreground",
        outline: "border-border bg-transparent text-foreground",
        muted: "border-border-subtle bg-muted text-muted-foreground",
        mint: "border-mint/30 bg-mint-surface text-mint",
        info: "border-sky-400/30 bg-sky-500/10 text-sky-300",
        success: "border-primary/40 bg-primary/15 text-primary",
        warning: "border-warning/40 bg-warning/10 text-warning",
        danger: "border-destructive/40 bg-destructive/10 text-destructive",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  },
);

function Badge({
  className,
  variant,
  ...props
}: ComponentProps<"span"> & VariantProps<typeof badgeVariants>) {
  return <span data-slot="badge" className={cn(badgeVariants({ variant }), className)} {...props} />;
}

type BadgeVariant = NonNullable<VariantProps<typeof badgeVariants>["variant"]>;

export { Badge, type BadgeVariant };
