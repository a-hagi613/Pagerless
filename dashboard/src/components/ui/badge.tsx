import type { ComponentProps } from "react";
import { cva, type VariantProps } from "class-variance-authority";

import { cn } from "@/lib/utils";

const badgeVariants = cva(
  "inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs font-medium whitespace-nowrap",
  {
    variants: {
      variant: {
        default: "border-transparent bg-slate-800 text-white",
        outline: "border-slate-300 bg-white text-slate-700",
        info: "border-sky-200 bg-sky-50 text-sky-800",
        success: "border-emerald-200 bg-emerald-50 text-emerald-800",
        warning: "border-amber-200 bg-amber-50 text-amber-800",
        danger: "border-rose-200 bg-rose-50 text-rose-800",
        muted: "border-slate-200 bg-slate-100 text-slate-600",
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
