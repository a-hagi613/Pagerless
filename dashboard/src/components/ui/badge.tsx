import { cva, type VariantProps } from "class-variance-authority";
import type { ComponentProps } from "react";

import { cn } from "@/lib/utils";

// Status language: a pill always carries an icon and a word beside its colour.
const badgeVariants = cva(
  "inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 font-mono text-xs leading-4 font-medium whitespace-nowrap",
  {
    variants: {
      variant: {
        default: "border-primary/40 bg-primary-soft text-primary-soft-foreground",
        secondary: "border-border bg-secondary text-secondary-foreground",
        outline: "border-border-strong bg-transparent text-foreground",
        muted: "border-border bg-muted text-muted-foreground",
        mint: "border-mint/40 bg-mint-surface text-mint",
        info: "border-info/40 bg-info-surface text-info",
        success: "border-primary/40 bg-primary-soft text-primary-soft-foreground",
        warning: "border-warning/40 bg-warning-surface text-warning",
        danger: "border-destructive/40 bg-danger-surface text-destructive",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  },
);

function Badge({ className, variant, ...props }: ComponentProps<"span"> & VariantProps<typeof badgeVariants>) {
  return <span data-slot="badge" className={cn(badgeVariants({ variant }), className)} {...props} />;
}

type BadgeVariant = NonNullable<VariantProps<typeof badgeVariants>["variant"]>;

export { Badge, type BadgeVariant };
